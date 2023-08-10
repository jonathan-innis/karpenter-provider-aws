package nodeclass

import (
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/v1beta1"
)

func New(nodeTemplate *v1alpha1.AWSNodeTemplate) *v1beta1.NodeClass {
	return &v1beta1.NodeClass{
		TypeMeta:   nodeTemplate.TypeMeta,
		ObjectMeta: nodeTemplate.ObjectMeta,
		Spec: v1beta1.NodeClassSpec{
			SubnetSelectorTerms: []v1beta1.SubnetSelectorTerm{
				{
					Tags: nodeTemplate.Spec.SubnetSelector,
				},
			},
			SecurityGroupSelectorTerms: []v1beta1.SecurityGroupSelectorTerm{
				{
					Tags: nodeTemplate.Spec.SecurityGroupSelector,
				},
			},
			AMISelectorTerms: []v1beta1.AMISelectorTerm{
				{
					Tags: nodeTemplate.Spec.AMISelector,
				},
			},
			AMIFamily: nodeTemplate.Spec.AMIFamily,
			UserData:  nodeTemplate.Spec.UserData,
		},
	}
}
