package deployer

import (
	"fmt"
	"github.com/DevopsArtFactory/goployer/pkg/aws"
	"github.com/DevopsArtFactory/goployer/pkg/builder"
	"github.com/DevopsArtFactory/goployer/pkg/schemas"
	"github.com/DevopsArtFactory/goployer/pkg/tool"
	Logger "github.com/sirupsen/logrus"
	"strings"
)

type BlueGreen struct {
	Deployer
}

func NewBlueGrean(mode string, logger *Logger.Logger, awsConfig schemas.AWSConfig, stack schemas.Stack) BlueGreen {
	awsClients := []aws.AWSClient{}
	for _, region := range stack.Regions {
		awsClients = append(awsClients, aws.BootstrapServices(region.Region, stack.AssumeRole))
	}
	return BlueGreen{
		Deployer{
			Mode:              mode,
			Logger:            logger,
			AwsConfig:         awsConfig,
			AWSClients:        awsClients,
			AsgNames:          map[string]string{},
			PrevAsgs:          map[string][]string{},
			PrevInstances:     map[string][]string{},
			PrevInstanceCount: map[string]schemas.Capacity{},
			PrevVersions:      map[string][]int{},
			Stack:             stack,
		},
	}
}

// Deploy function
func (b BlueGreen) Deploy(config builder.Config) error {
	b.Logger.Info("Deploy Mode is " + b.Mode)

	//Get LocalFileProvider
	b.LocalProvider = builder.SetUserdataProvider(b.Stack.Userdata, b.AwsConfig.Userdata)

	// Make Frigga
	frigga := tool.Frigga{}
	for _, region := range b.Stack.Regions {
		//Region check
		//If region id is passed from command line, then deployer will deploy in that region only.
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			return err
		}

		//Setup frigga with prefix
		frigga.Prefix = tool.BuildPrefixName(b.AwsConfig.Name, b.Stack.Env, region.Region)

		// Get Current Version
		curVersion := getCurrentVersion(b.PrevVersions[region.Region])
		b.Logger.Info("Current Version :", curVersion)

		//Get AMI
		var ami string
		if len(config.Ami) > 0 {
			ami = config.Ami
		} else {
			ami = region.AmiId
		}

		// Generate new name for autoscaling group and launch configuration
		new_asg_name := tool.GenerateAsgName(frigga.Prefix, curVersion)
		launch_template_name := tool.GenerateLcName(new_asg_name)

		userdata := (b.LocalProvider).Provide()

		//Stack check
		securityGroups := client.EC2Service.GetSecurityGroupList(region.VPC, region.SecurityGroups)
		blockDevices := client.EC2Service.MakeLaunchTemplateBlockDeviceMappings(b.Stack.BlockDevices)
		ebsOptimized := b.Stack.EbsOptimized

		// Instance Type Override
		instanceType := region.InstanceType
		if len(config.OverrideInstanceType) > 0 {
			instanceType = config.OverrideInstanceType

			if b.Stack.MixedInstancesPolicy.Enabled {
				Logger.Warnf("--override-instance-type won't be applied because mixed_instances_policy is enabled")
			}
		}

		// LaunchTemplate
		ret := client.EC2Service.CreateNewLaunchTemplate(
			launch_template_name,
			ami,
			instanceType,
			region.SshKey,
			b.Stack.IamInstanceProfile,
			userdata,
			ebsOptimized,
			b.Stack.MixedInstancesPolicy.Enabled,
			securityGroups,
			blockDevices,
			b.Stack.InstanceMarketOptions,
		)

		if !ret {
			return fmt.Errorf("unknown error happened creating new launch template")
		}

		healthElb := region.HealthcheckLB
		loadbalancers := region.LoadBalancers
		if healthElb != "" && !tool.IsStringInArray(healthElb, loadbalancers) {
			loadbalancers = append(loadbalancers, healthElb)
		}

		healthcheckTargetGroup := region.HealthcheckTargetGroup
		targetGroups := region.TargetGroups
		if healthcheckTargetGroup != "" && !tool.IsStringInArray(healthcheckTargetGroup, targetGroups) {
			targetGroups = append(targetGroups, healthcheckTargetGroup)
		}

		usePublicSubnets := region.UsePublicSubnets
		healthcheckType := aws.DEFAULT_HEALTHCHECK_TYPE
		healthcheckGracePeriod := int64(aws.DEFAULT_HEALTHCHECK_GRACE_PERIOD)
		terminationPolicies := []*string{}
		availabilityZones := client.EC2Service.GetAvailabilityZones(region.VPC, region.AvailabilityZones)
		tags := client.EC2Service.GenerateTags(b.AwsConfig.Tags, new_asg_name, b.AwsConfig.Name, config.Stack, b.Stack.AnsibleTags, b.Stack.Tags, config.ExtraTags, config.AnsibleExtraVars, region.Region)
		subnets := client.EC2Service.GetSubnets(region.VPC, usePublicSubnets, availabilityZones)
		lifecycleHooksSpecificationList := client.EC2Service.GenerateLifecycleHooks(b.Stack.LifecycleHooks)
		targetGroupArns, err := client.ELBV2Service.GetTargetGroupARNs(targetGroups)
		if err != nil {
			return err
		}

		var appliedCapacity schemas.Capacity
		if !config.ForceManifestCapacity && b.PrevInstanceCount[region.Region].Desired > b.Stack.Capacity.Desired {
			appliedCapacity = b.PrevInstanceCount[region.Region]
			b.Logger.Infof("Current desired instance count is larger than the number of instances in manifest file")
		} else {
			appliedCapacity = b.Stack.Capacity
		}

		b.Logger.Infof("Applied instance capacity - Min: %d, Desired: %d, Max: %d", appliedCapacity.Min, appliedCapacity.Desired, appliedCapacity.Max)

		ret, err = client.EC2Service.CreateAutoScalingGroup(
			new_asg_name,
			launch_template_name,
			healthcheckType,
			healthcheckGracePeriod,
			appliedCapacity,
			aws.MakeStringArrayToAwsStrings(loadbalancers),
			targetGroupArns,
			terminationPolicies,
			aws.MakeStringArrayToAwsStrings(availabilityZones),
			tags,
			subnets,
			b.Stack.MixedInstancesPolicy,
			lifecycleHooksSpecificationList,
		)

		if err != nil {
			return err
		}

		b.AsgNames[region.Region] = new_asg_name

		if b.Collector.MetricConfig.Enabled {
			additionalFields := map[string]string{}
			if len(config.ReleaseNotes) > 0 {
				additionalFields["release-notes"] = config.ReleaseNotes
			}

			if len(config.ReleaseNotesBase64) > 0 {
				additionalFields["release-notes-base64"] = config.ReleaseNotesBase64
			}

			if len(userdata) > 0 {
				additionalFields["userdata"] = userdata
			}

			b.Stack.Capacity = appliedCapacity
			b.Collector.StampDeployment(b.Stack, config, tags, new_asg_name, "creating", additionalFields)
		}
	}

	return nil
}

// Healthchecking
func (b BlueGreen) HealthChecking(config builder.Config) map[string]bool {
	stack_name := b.GetStackName()
	Logger.Debug(fmt.Sprintf("Healthchecking for stack starts : %s", stack_name))
	finished := []string{}

	//Valid Count
	validCount := 1
	if config.Region == "" {
		validCount = len(b.Stack.Regions)
	}

	if len(config.Region) > 0 {
		if !CheckRegionExist(config.Region, b.Stack.Regions) {
			validCount = 0
		}
	}

	for _, region := range b.Stack.Regions {
		//Region check
		//If region id is passed from command line, then deployer will deploy in that region only.
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		b.Logger.Debug("Healthchecking for region starts : " + region.Region)

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			return map[string]bool{stack_name: false, "error": true}
		}

		asg, err := client.EC2Service.GetMatchingAutoscalingGroup(b.AsgNames[region.Region])
		if err != nil {
			return map[string]bool{stack_name: false, "error": true}
		}

		isHealthy, err := b.Deployer.polling(region, asg, client)
		if err != nil {
			return map[string]bool{stack_name: false, "error": true}
		}

		if isHealthy {
			if b.Collector.MetricConfig.Enabled {
				if err := b.Collector.UpdateStatus(*asg.AutoScalingGroupName, "deployed", nil); err != nil {
					Logger.Errorf("Update status Error, %s : %s", err.Error(), *asg.AutoScalingGroupName)
				}
			}
			finished = append(finished, region.Region)
		}
	}

	if len(finished) == validCount {
		return map[string]bool{stack_name: true, "error": false}
	}

	return map[string]bool{stack_name: false, "error": false}
}

//Stack Name Getter
func (b BlueGreen) GetStackName() string {
	return b.Stack.Stack
}

//BlueGreen finish final work
func (b BlueGreen) FinishAdditionalWork(config builder.Config) error {
	if len(b.Stack.Autoscaling) == 0 {
		b.Logger.Debug("No scaling policy exists")
		return nil
	}

	if len(config.Region) > 0 && !CheckRegionExist(config.Region, b.Stack.Regions) {
		return nil
	}

	//Apply AutoScaling Policies
	for _, region := range b.Stack.Regions {
		//If region id is passed from command line, then deployer will deploy in that region only.
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		b.Logger.Info("Attaching autoscaling policies : " + region.Region)

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			tool.ErrorLogging(err.Error())
		}

		//putting autoscaling group policies
		policies := []string{}
		policyArns := map[string]string{}
		for _, policy := range b.Stack.Autoscaling {
			policyArn, err := client.EC2Service.CreateScalingPolicy(policy, b.AsgNames[region.Region])
			if err != nil {
				tool.ErrorLogging(err.Error())
				return err
			}
			policyArns[policy.Name] = *policyArn
			policies = append(policies, policy.Name)
		}

		if err := client.EC2Service.EnableMetrics(b.AsgNames[region.Region]); err != nil {
			return err
		}

		if err := client.CloudWatchService.CreateScalingAlarms(b.AsgNames[region.Region], b.Stack.Alarms, policyArns); err != nil {
			return nil
		}
	}

	Logger.Debug("Finish addtional works.")
	return nil
}

// Run lifecycle callbacks before cleaninig.
func (b BlueGreen) TriggerLifecycleCallbacks(config builder.Config) error {
	if &b.Stack.LifecycleCallbacks == nil || len(b.Stack.LifecycleCallbacks.PreTerminatePastClusters) == 0 {
		b.Logger.Debugf("no lifecycle callbacks in %s\n", b.Stack.Stack)
		return nil
	}

	if len(config.Region) > 0 {
		if !CheckRegionExist(config.Region, b.Stack.Regions) {
			b.Logger.Debugf("region [ %s ] is not in the stack [ %s ].", config.Region, b.Stack.Stack)
			return nil
		}
	}

	for _, region := range b.Stack.Regions {
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			tool.ErrorLogging(err.Error())
		}

		if len(b.PrevInstances[region.Region]) > 0 {
			b.Deployer.RunLifecycleCallbacks(client, b.PrevInstances[region.Region])
		} else {

			b.Logger.Infof("No previous versions to be deleted : %s\n", region.Region)
			b.Slack.SendSimpleMessage(fmt.Sprintf("No previous versions to be deleted : %s\n", region.Region), config.Env)
		}

	}
	return nil
}

//Clean Previous Version
func (b BlueGreen) CleanPreviousVersion(config builder.Config) error {
	b.Logger.Debug("Delete Mode is " + b.Mode)

	if len(config.Region) > 0 {
		if !CheckRegionExist(config.Region, b.Stack.Regions) {
			return nil
		}
	}

	for _, region := range b.Stack.Regions {
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		b.Logger.Infof("[%s]The number of previous versions to delete is %d", region.Region, len(b.PrevAsgs[region.Region]))

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			tool.ErrorLogging(err.Error())
		}

		// First make autoscaling group size to 0
		if len(b.PrevAsgs[region.Region]) > 0 {
			for _, asg := range b.PrevAsgs[region.Region] {
				b.Logger.Debugf("[Resizing to 0] target autoscaling group : %s", asg)
				if err := b.ResizingAutoScalingGroupToZero(client, b.Stack.Stack, asg); err != nil {
					b.Logger.Errorf(err.Error())
				}
			}
		} else {
			b.Logger.Infof("No previous versions to be deleted : %s\n", region.Region)
			b.Slack.SendSimpleMessage(fmt.Sprintf("No previous versions to be deleted : %s\n", region.Region), config.Env)
		}
	}

	return nil
}

// Clean Teramination Checking
func (b BlueGreen) TerminateChecking(config builder.Config) map[string]bool {
	stack_name := b.GetStackName()
	Logger.Info(fmt.Sprintf("Termination Checking for %s starts...", stack_name))

	//Valid Count
	validCount := 1
	if config.Region == "" {
		validCount = len(b.Stack.Regions)
	}

	if len(config.Region) > 0 {
		if !CheckRegionExist(config.Region, b.Stack.Regions) {
			validCount = 0
		}
	}

	finished := []string{}
	for _, region := range b.Stack.Regions {
		//Region check
		//If region id is passed from command line, then deployer will deploy in that region only.
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		b.Logger.Info("Checking Termination stack for region starts : " + region.Region)

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			tool.ErrorLogging(err.Error())
		}

		targets := b.PrevAsgs[region.Region]
		if len(targets) == 0 {
			Logger.Info("No target to delete : ", region.Region)
			finished = append(finished, region.Region)
			continue
		}

		ok_count := 0
		for _, target := range targets {
			ok := b.Deployer.CheckTerminating(client, target, config.DisableMetrics)
			if ok {
				Logger.Info("finished : ", target)
				ok_count++
			}
		}

		if ok_count == len(targets) {
			finished = append(finished, region.Region)
		}
	}

	if len(finished) == validCount {
		return map[string]bool{stack_name: true}
	}

	return map[string]bool{stack_name: false}
}

// CheckRegionExist checks if target region is really in regions described in manifest file
func CheckRegionExist(target string, regions []schemas.RegionConfig) bool {
	regionExists := false
	for _, region := range regions {
		if region.Region == target {
			regionExists = true
			break
		}
	}

	return regionExists
}

// Gather the whole metrics from deployer
func (b BlueGreen) GatherMetrics(config builder.Config) error {
	if config.DisableMetrics {
		return nil
	}

	if len(config.Region) > 0 {
		if !CheckRegionExist(config.Region, b.Stack.Regions) {
			return nil
		}
	}

	for _, region := range b.Stack.Regions {
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		b.Logger.Infof("[%s]The number of previous autoscaling groups for gathering metrics is %d", region.Region, len(b.PrevAsgs[region.Region]))

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			tool.ErrorLogging(err.Error())
		}

		if len(b.PrevAsgs[region.Region]) > 0 {
			var errors []error
			for _, asg := range b.PrevAsgs[region.Region] {
				b.Logger.Debugf("Start gathering metrics about autoscaling group : %s", asg)
				err := b.Deployer.GatherMetrics(client, asg)
				if err != nil {
					errors = append(errors, err)
				}
				b.Logger.Debugf("Finish gathering metrics about autoscaling group %s.", asg)
			}

			if len(errors) > 0 {
				for _, e := range errors {
					b.Logger.Errorf(e.Error())
				}
				return fmt.Errorf("error occurred on gathering metrics")
			}
		} else {
			b.Logger.Debugf("No previous versions to gather metrics : %s\n", region.Region)
		}
	}

	return nil
}

// Deploy function
func (b BlueGreen) CheckPrevious(config builder.Config) error {
	// Make Frigga
	frigga := tool.Frigga{}
	for _, region := range b.Stack.Regions {
		//Region check
		//If region id is passed from command line, then deployer will deploy in that region only.
		if config.Region != "" && config.Region != region.Region {
			b.Logger.Debug("This region is skipped by user : " + region.Region)
			continue
		}

		//Setup frigga with prefix
		frigga.Prefix = tool.BuildPrefixName(b.AwsConfig.Name, b.Stack.Env, region.Region)

		//select client
		client, err := selectClientFromList(b.AWSClients, region.Region)
		if err != nil {
			return err
		}

		// Get All Autoscaling Groups
		asgGroups := client.EC2Service.GetAllMatchingAutoscalingGroupsWithPrefix(frigga.Prefix)

		//Get All Previous Autoscaling Groups and versions
		prevAsgs := []string{}
		prevInstanceIds := []string{}
		prevVersions := []int{}
		var prevInstanceCount schemas.Capacity
		for _, asgGroup := range asgGroups {
			prevAsgs = append(prevAsgs, *asgGroup.AutoScalingGroupName)
			prevVersions = append(prevVersions, tool.ParseVersion(*asgGroup.AutoScalingGroupName))
			for _, instance := range asgGroup.Instances {
				prevInstanceIds = append(prevInstanceIds, *instance.InstanceId)
			}

			prevInstanceCount.Desired = *asgGroup.DesiredCapacity
			prevInstanceCount.Max = *asgGroup.MaxSize
			prevInstanceCount.Min = *asgGroup.MinSize
		}
		b.Logger.Info("Previous Versions : ", strings.Join(prevAsgs, " | "))

		b.PrevAsgs[region.Region] = prevAsgs
		b.PrevInstances[region.Region] = prevInstanceIds
		b.PrevVersions[region.Region] = prevVersions
		b.PrevInstanceCount[region.Region] = prevInstanceCount
	}

	return nil
}
