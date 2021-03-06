package aws

import (
	"fmt"
	"github.com/DevopsArtFactory/goployer/pkg/tool"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/elbv2"
	Logger "github.com/sirupsen/logrus"
)

type ELBV2Client struct {
	Client *elbv2.ELBV2
}

type HealthcheckHost struct {
	InstanceId     string
	LifecycleState string
	TargetStatus   string
	HealthStatus   string
	Healthy        bool
}

func NewELBV2Client(session *session.Session, region string, creds *credentials.Credentials) ELBV2Client {
	return ELBV2Client{
		Client: getElbClientFn(session, region, creds),
	}
}

func getElbClientFn(session *session.Session, region string, creds *credentials.Credentials) *elbv2.ELBV2 {
	if creds == nil {
		return elbv2.New(session, &aws.Config{Region: aws.String(region)})
	}
	return elbv2.New(session, &aws.Config{Region: aws.String(region), Credentials: creds})
}

// GetTargetGroupARNs returns arn list of target groups
func (e ELBV2Client) GetTargetGroupARNs(target_groups []string) ([]*string, error) {
	if len(target_groups) == 0 {
		return nil, nil
	}

	input := &elbv2.DescribeTargetGroupsInput{
		Names: MakeStringArrayToAwsStrings(target_groups),
	}

	result, err := e.Client.DescribeTargetGroups(input)
	if err != nil {
		return nil, err
	}

	tgs := []*string{}
	for _, group := range result.TargetGroups {
		tgs = append(tgs, group.TargetGroupArn)
	}

	return tgs, nil
}

// GetHostInTarget gets host instance
func (e ELBV2Client) GetHostInTarget(group *autoscaling.Group, target_group_arn *string) ([]HealthcheckHost, error) {
	Logger.Debug(fmt.Sprintf("[Checking healthy host count] Autoscaling Group: %s", *group.AutoScalingGroupName))

	input := &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(*target_group_arn),
	}

	result, err := e.Client.DescribeTargetHealth(input)
	if err != nil {
		return nil, err
	}

	ret := []HealthcheckHost{}
	for _, instance := range group.Instances {
		target_state := tool.INITIAL_STATUS
		for _, hd := range result.TargetHealthDescriptions {
			if *hd.Target.Id == *instance.InstanceId {
				target_state = *hd.TargetHealth.State
				break
			}
		}

		ret = append(ret, HealthcheckHost{
			InstanceId:     *instance.InstanceId,
			LifecycleState: *instance.LifecycleState,
			TargetStatus:   target_state,
			HealthStatus:   *instance.HealthStatus,
			Healthy:        *instance.LifecycleState == "InService" && target_state == "healthy" && *instance.HealthStatus == "Healthy",
		})
	}
	return ret, nil
}
