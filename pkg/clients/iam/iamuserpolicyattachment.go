package iam

import (
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/aws/aws-sdk-go-v2/service/iam"

	"github.com/crossplane/provider-aws/apis/identity/v1alpha1"
	awsclients "github.com/crossplane/provider-aws/pkg/clients"
)

// UserPolicyAttachmentClient is the external client used for UserPolicyAttachment Custom Resource
type UserPolicyAttachmentClient interface {
	AttachUserPolicyRequest(*iam.AttachUserPolicyInput) iam.AttachUserPolicyRequest
	DetachUserPolicyRequest(*iam.DetachUserPolicyInput) iam.DetachUserPolicyRequest
	ListAttachedUserPoliciesRequest(*iam.ListAttachedUserPoliciesInput) iam.ListAttachedUserPoliciesRequest
}

// NewUserPolicyAttachmentClient creates new RDS RDSClient with provided AWS Configurations/Credentials
func NewUserPolicyAttachmentClient(cfg aws.Config) UserPolicyAttachmentClient {
	return iam.New(cfg)
}

// LateInitializeUserPolicy fills the empty fields in v1alpha1.UserPolicyAttachmentParameters with
// the values seen in iam.AttachedPolicy.
func LateInitializeUserPolicy(in *v1alpha1.IAMUserPolicyAttachmentParameters, policy *iam.AttachedPolicy) {
	if policy == nil {
		return
	}
	in.PolicyARN = awsclients.LateInitializeString(in.PolicyARN, policy.PolicyArn)
}
