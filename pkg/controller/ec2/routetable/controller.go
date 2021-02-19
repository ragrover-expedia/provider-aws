/*
Copyright 2019 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package routetable

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/crossplane/provider-aws/apis/ec2/v1alpha4"
	"github.com/crossplane/provider-aws/apis/ec2/v1beta1"
	awsclient "github.com/crossplane/provider-aws/pkg/clients"
	"github.com/crossplane/provider-aws/pkg/clients/ec2"
)

const (
	errUnexpectedObject = "The managed resource is not an RouteTable resource"

	errDescribe           = "failed to describe RouteTable"
	errMultipleItems      = "retrieved multiple RouteTables for the given routeTableId"
	errCreate             = "failed to create the RouteTable resource"
	errUpdate             = "failed to update the RouteTable"
	errUpdateNotFound     = "cannot update the RouteTable, since the RouteTableID is not present"
	errDelete             = "failed to delete the RouteTable resource"
	errCreateRoute        = "failed to create a route in the RouteTable resource"
	errAssociateSubnet    = "failed to associate subnet %v to the RouteTable resource"
	errDisassociateSubnet = "failed to disassociate subnet %v from the RouteTable resource"
	errCreateTags         = "failed to create tags for the RouteTable resource"
)

// SetupRouteTable adds a controller that reconciles RouteTables.
func SetupRouteTable(mgr ctrl.Manager, l logging.Logger, rl workqueue.RateLimiter) error {
	name := managed.ControllerName(v1alpha4.RouteTableGroupKind)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(controller.Options{
			RateLimiter: ratelimiter.NewDefaultManagedRateLimiter(rl),
		}).
		For(&v1alpha4.RouteTable{}).
		Complete(managed.NewReconciler(mgr,
			resource.ManagedKind(v1alpha4.RouteTableGroupVersionKind),
			managed.WithExternalConnecter(&connector{kube: mgr.GetClient(), newClientFn: ec2.NewRouteTableClient}),
			managed.WithReferenceResolver(managed.NewAPISimpleReferenceResolver(mgr.GetClient())),
			managed.WithInitializers(managed.NewDefaultProviderConfig(mgr.GetClient())),
			managed.WithConnectionPublishers(),
			managed.WithLogger(l.WithValues("controller", name)),
			managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name)))))
}

type connector struct {
	kube        client.Client
	newClientFn func(config aws.Config) ec2.RouteTableClient
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha4.RouteTable)
	if !ok {
		return nil, errors.New(errUnexpectedObject)
	}
	cfg, err := awsclient.GetConfig(ctx, c.kube, mg, cr.Spec.ForProvider.Region)
	if err != nil {
		return nil, err
	}
	return &external{client: c.newClientFn(*cfg), kube: c.kube}, nil
}

type external struct {
	kube   client.Client
	client ec2.RouteTableClient
}

func (e *external) Observe(ctx context.Context, mgd resource.Managed) (managed.ExternalObservation, error) { // nolint:gocyclo
	cr, ok := mgd.(*v1alpha4.RouteTable)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errUnexpectedObject)
	}

	// To find out whether a RouteTable exist:
	// - the object's ExternalName should have routeTableId populated
	// - a RouteTable with the given routeTableId should exist
	if meta.GetExternalName(cr) == "" {
		return managed.ExternalObservation{
			ResourceExists: false,
		}, nil
	}

	response, err := e.client.DescribeRouteTablesRequest(&awsec2.DescribeRouteTablesInput{
		RouteTableIds: []string{meta.GetExternalName(cr)},
	}).Send(ctx)

	if err != nil {
		return managed.ExternalObservation{}, awsclient.Wrap(resource.Ignore(ec2.IsRouteTableNotFoundErr, err), errDescribe)
	}

	// in a successful response, there should be one and only one object
	if len(response.RouteTables) != 1 {
		return managed.ExternalObservation{}, errors.New(errMultipleItems)
	}

	observed := response.RouteTables[0]
	current := cr.Spec.ForProvider.DeepCopy()
	ec2.LateInitializeRT(&cr.Spec.ForProvider, &response.RouteTables[0])

	stateAvailable := true
	for _, rt := range observed.Routes {
		if rt.State != awsec2.RouteStateActive {
			stateAvailable = false
			break
		}
	}
	if stateAvailable {
		cr.SetConditions(xpv1.Available())
	}

	cr.Status.AtProvider = ec2.GenerateRTObservation(observed)

	upToDate, err := ec2.IsRtUpToDate(cr.Spec.ForProvider, observed)
	if err != nil {
		return managed.ExternalObservation{}, awsclient.Wrap(err, errDescribe)
	}

	return managed.ExternalObservation{
		ResourceExists:          true,
		ResourceUpToDate:        upToDate,
		ResourceLateInitialized: !cmp.Equal(current, &cr.Spec.ForProvider),
	}, nil
}

func (e *external) Create(ctx context.Context, mgd resource.Managed) (managed.ExternalCreation, error) { // nolint:gocyclo
	cr, ok := mgd.(*v1alpha4.RouteTable)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errUnexpectedObject)
	}
	result, err := e.client.CreateRouteTableRequest(&awsec2.CreateRouteTableInput{
		VpcId: cr.Spec.ForProvider.VPCID,
	}).Send(ctx)
	if err != nil {
		return managed.ExternalCreation{}, awsclient.Wrap(err, errCreate)
	}
	meta.SetExternalName(cr, aws.StringValue(result.RouteTable.RouteTableId))
	return managed.ExternalCreation{ExternalNameAssigned: true}, nil
}

func (e *external) Update(ctx context.Context, mgd resource.Managed) (managed.ExternalUpdate, error) { // nolint:gocyclo
	cr, ok := mgd.(*v1alpha4.RouteTable)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errUnexpectedObject)
	}

	response, err := e.client.DescribeRouteTablesRequest(&awsec2.DescribeRouteTablesInput{
		RouteTableIds: []string{meta.GetExternalName(cr)},
	}).Send(ctx)

	if err != nil {
		return managed.ExternalUpdate{}, awsclient.Wrap(resource.Ignore(ec2.IsRouteTableNotFoundErr, err), errDescribe)
	}

	if response.RouteTables == nil {
		return managed.ExternalUpdate{}, awsclient.Wrap(err, errUpdateNotFound)
	}

	table := response.RouteTables[0]

	patch, err := ec2.CreateRTPatch(table, cr.Spec.ForProvider)
	if err != nil {
		return managed.ExternalUpdate{}, awsclient.Wrap(err, errUpdate)
	}

	if len(patch.Tags) != 0 {
		// tagging the RouteTable
		if _, err := e.client.CreateTagsRequest(&awsec2.CreateTagsInput{
			Resources: []string{meta.GetExternalName(cr)},
			Tags:      v1beta1.GenerateEC2Tags(cr.Spec.ForProvider.Tags),
		}).Send(ctx); err != nil {
			return managed.ExternalUpdate{}, awsclient.Wrap(err, errCreateTags)
		}
	}

	if patch.Routes != nil {
		// Attach the routes in Spec
		if err := e.createRoutes(ctx, meta.GetExternalName(cr), cr.Spec.ForProvider.Routes, cr.Status.AtProvider.Routes); err != nil {
			return managed.ExternalUpdate{}, err
		}
	}

	if patch.Associations != nil {
		// Associate route table to subnets in Spec.
		if err := e.createAssociations(ctx, meta.GetExternalName(cr), cr.Spec.ForProvider.Associations, cr.Status.AtProvider.Associations); err != nil {
			return managed.ExternalUpdate{}, err
		}
	}

	return managed.ExternalUpdate{}, nil
}

func (e *external) Delete(ctx context.Context, mgd resource.Managed) error {
	cr, ok := mgd.(*v1alpha4.RouteTable)
	if !ok {
		return errors.New(errUnexpectedObject)
	}

	cr.Status.SetConditions(xpv1.Deleting())

	// the subnet associations have to be deleted before deleting the route table.
	if err := e.deleteAssociations(ctx, cr.Status.AtProvider.Associations); err != nil {
		return err
	}

	_, err := e.client.DeleteRouteTableRequest(&awsec2.DeleteRouteTableInput{
		RouteTableId: aws.String(meta.GetExternalName(cr)),
	}).Send(ctx)

	return awsclient.Wrap(resource.Ignore(ec2.IsRouteTableNotFoundErr, err), errDelete)
}

func (e *external) createRoutes(ctx context.Context, tableID string, desired []v1alpha4.Route, observed []v1alpha4.RouteState) error {
	for _, rt := range desired {
		isObserved := false
		for _, ob := range observed {
			if ob.GatewayID == aws.StringValue(rt.GatewayID) && ob.DestinationCIDRBlock == aws.StringValue(rt.DestinationCIDRBlock) {
				isObserved = true
				break
			}
		}
		// if the route is already created, skip it
		if !isObserved {
			_, err := e.client.CreateRouteRequest(&awsec2.CreateRouteInput{
				RouteTableId:         aws.String(tableID),
				DestinationCidrBlock: rt.DestinationCIDRBlock,
				GatewayId:            rt.GatewayID,
			}).Send(ctx)

			if err != nil {
				return awsclient.Wrap(err, errCreateRoute)
			}
		}
	}

	return nil
}

func (e *external) createAssociations(ctx context.Context, tableID string, desired []v1alpha4.Association, observed []v1alpha4.AssociationState) error {
	for _, asc := range desired {
		isObserved := false
		for _, ob := range observed {
			if ob.SubnetID == aws.StringValue(asc.SubnetID) {
				isObserved = true
				break
			}
		}
		// if the association is already created, skip it
		if !isObserved {
			_, err := e.client.AssociateRouteTableRequest(&awsec2.AssociateRouteTableInput{
				RouteTableId: aws.String(tableID),
				SubnetId:     asc.SubnetID,
			}).Send(ctx)

			if err != nil {
				return awsclient.Wrap(err, errAssociateSubnet)
			}
		}
	}

	return nil
}

func (e *external) deleteAssociations(ctx context.Context, observed []v1alpha4.AssociationState) error {
	for _, asc := range observed {
		req := e.client.DisassociateRouteTableRequest(&awsec2.DisassociateRouteTableInput{
			AssociationId: aws.String(asc.AssociationID),
		})

		if _, err := req.Send(ctx); err != nil {
			if ec2.IsAssociationIDNotFoundErr(err) {
				continue
			}
			return awsclient.Wrap(err, errDisassociateSubnet)
		}
	}

	return nil
}
