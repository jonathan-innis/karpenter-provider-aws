package cloudprovider

import (
	"fmt"

	"github.com/avast/retry-go"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

type VolumeProvider struct {
	ec2api ec2iface.EC2API
}

func NewVolumeProvider(ec2api ec2iface.EC2API) *VolumeProvider {
	return &VolumeProvider{
		ec2api: ec2api,
	}
}

// GetEphemeralVolume retrieves the first blockDeviceMapping volume from the instance, which we assume
// to be the ephemeral volume for the node
func (p *VolumeProvider) GetEphemeralVolume(instance *ec2.Instance) (*ec2.Volume, error) {
	var volume *ec2.Volume
	if len(instance.BlockDeviceMappings) == 0 || instance.BlockDeviceMappings[0].Ebs == nil {
		return nil, fmt.Errorf("no block device mapping exists")
	}
	if err := retry.Do(func() error {
		out, err := p.ec2api.DescribeVolumes(&ec2.DescribeVolumesInput{
			VolumeIds: []*string{instance.BlockDeviceMappings[0].Ebs.VolumeId},
		})
		if err != nil {
			return fmt.Errorf("getting instance volumes, %w", err)
		}
		if len(out.Volumes) != 1 {
			return fmt.Errorf("expected a single device volume, got %d", len(out.Volumes))
		}
		volume = out.Volumes[0] // First volume in the response
		return nil
	}); err != nil {
		return nil, fmt.Errorf("retrieving device volume for instance, %w", err)
	}
	return volume, nil
}
