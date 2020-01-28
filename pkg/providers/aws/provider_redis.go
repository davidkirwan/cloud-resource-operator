package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/aws-sdk-go/service/sts/stsiface"
	"github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1/types"

	prometheusv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	monitoringclient "github.com/coreos/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	croType "github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1/types"
	croResources "github.com/integr8ly/cloud-resource-operator/pkg/resources"

	"github.com/aws/aws-sdk-go/service/elasticache/elasticacheiface"
	"github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elasticache"
	"github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/cloud-resource-operator/pkg/resources"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/integr8ly/cloud-resource-operator/pkg/providers"

	errorUtil "github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultRedisMaintenanceMetricName = "cro_aws_elasticache_service_maintenance"
	defaultRedisInfoMetricName        = "cro_aws_elasticache_info"
	defaultRedisAvailMetricName       = "cro_aws_elasticache_available"
	redisProviderName                 = "aws-elasticache"
	// default create params
	defaultCacheNodeType     = "cache.t2.micro"
	defaultEngineVersion     = "3.2.10"
	defaultDescription       = "A Redis replication group"
	defaultNumCacheClusters  = 2
	defaultSnapshotRetention = 30
)

var _ providers.RedisProvider = (*RedisProvider)(nil)

// AWS Redis Provider implementation for AWS Elasticache
type RedisProvider struct {
	Client            client.Client
	Logger            *logrus.Entry
	CredentialManager CredentialManager
	ConfigManager     ConfigManager
	CacheSvc          elasticacheiface.ElastiCacheAPI
	MonClientV1       monitoringclient.MonitoringV1Interface
}

func NewAWSRedisProvider(client client.Client, logger *logrus.Entry) *RedisProvider {
	return &RedisProvider{
		Client:            client,
		Logger:            logger.WithFields(logrus.Fields{"provider": redisProviderName}),
		CredentialManager: NewCredentialMinterCredentialManager(client),
		ConfigManager:     NewDefaultConfigMapConfigManager(client),
	}
}

func (p *RedisProvider) GetName() string {
	return redisProviderName
}

func (p *RedisProvider) SupportsStrategy(d string) bool {
	return d == providers.AWSDeploymentStrategy
}

func (p *RedisProvider) GetReconcileTime(r *v1alpha1.Redis) time.Duration {
	if r.Status.Phase != croType.PhaseComplete {
		return time.Second * 60
	}
	return resources.GetForcedReconcileTimeOrDefault(defaultReconcileTime)
}

// CreateRedis Create an Elasticache Replication Group from strategy config
func (p *RedisProvider) CreateRedis(ctx context.Context, r *v1alpha1.Redis) (*providers.RedisCluster, croType.StatusMessage, error) {
	// handle provider-specific finalizer
	if err := resources.CreateFinalizer(ctx, p.Client, r, DefaultFinalizer); err != nil {
		return nil, "failed to set finalizer", err
	}

	// info about the elasticache cluster to be created
	elasticacheCreateConfig, _, stratCfg, err := p.getElasticacheConfig(ctx, r)
	if err != nil {
		errMsg := fmt.Sprintf("failed to retrieve aws elasticache cluster config %s", r.Name)
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
	}

	// create the credentials to be used by the aws resource providers, not to be used by end-user
	providerCreds, err := p.CredentialManager.ReconcileProviderCredentials(ctx, r.Namespace)
	if err != nil {
		msg := "failed to reconcile elasticache credentials"
		return nil, croType.StatusMessage(msg), errorUtil.Wrap(err, msg)
	}

	// setup aws elasticache cluster sdk session
	cacheSvc, stsSvc := createAWSService(stratCfg, providerCreds)

	// create the aws elasticache cluster
	return p.createElasticacheCluster(ctx, r, cacheSvc, stsSvc, elasticacheCreateConfig, stratCfg)
}

func createAWSService(stratCfg *StrategyConfig, providerCreds *Credentials) (elasticacheiface.ElastiCacheAPI, stsiface.STSAPI) {
	sess := session.Must(session.NewSession(&aws.Config{
		Region:      aws.String(stratCfg.Region),
		Credentials: credentials.NewStaticCredentials(providerCreds.AccessKeyID, providerCreds.SecretAccessKey, ""),
	}))

	return elasticache.New(sess), sts.New(sess)
}

func (p *RedisProvider) createElasticacheCluster(ctx context.Context, r *v1alpha1.Redis, cacheSvc elasticacheiface.ElastiCacheAPI, stsSvc stsiface.STSAPI, elasticacheConfig *elasticache.CreateReplicationGroupInput, stratCfg *StrategyConfig) (*providers.RedisCluster, types.StatusMessage, error) {
	// the aws access key can sometimes still not be registered in aws on first try, so loop
	rgs, err := getReplicationGroups(cacheSvc)
	if err != nil {
		// return nil error so this function can be requeueed
		errMsg := "error getting replication groups"
		logrus.Info(errMsg, err)
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
	}

	// verify and build elasticache create config
	if err := p.buildElasticacheCreateStrategy(ctx, r, elasticacheConfig); err != nil {
		errMsg := "failed to build and verify aws elasticache create strategy"
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// check if the cluster has already been created
	var foundCache *elasticache.ReplicationGroup
	for _, c := range rgs {
		if *c.ReplicationGroupId == *elasticacheConfig.ReplicationGroupId {
			foundCache = c
			break
		}
	}

	// create elasticache cluster if it doesn't exist
	if foundCache == nil {
		logrus.Info("creating elasticache cluster")
		if _, err = cacheSvc.CreateReplicationGroup(elasticacheConfig); err != nil {
			errMsg := fmt.Sprintf("error creating elasticache cluster %s", err)
			return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
		}
		return nil, "started elasticache provision", nil
	}

	err = p.setRedisServiceMaintenanceMetric(ctx, r, cacheSvc, foundCache)
	if err != nil {
		errMsg := "error creating the elasticache service maintenance metrics"
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// set status metric
	if err := p.exposeRedisMetrics(ctx, r, foundCache); err != nil {
		return nil, "failed to set metric", err
	}

	// check elasticache phase
	if *foundCache.Status != "available" {
		return nil, croType.StatusMessage(fmt.Sprintf("createReplicationGroup() in progress, current aws elasticache status is %s", *foundCache.Status)), nil
	}

	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		errMsg := "failed to retrieve cluster identifier"
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}
	// Create the PrometheusRule alert to watch for the availability of this ElastiCache instance we are provisioning
	err = p.CreateElastiCacheAvailabilityAlert(ctx, r, *foundCache.ReplicationGroupId, clusterID)
	if err != nil {
		errMsg := "error creating the elasticache PrometheusRule"
		return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// check if found cluster and user strategy differs, and modify instance
	logrus.Info("found existing elasticache instance")
	ec := buildElasticacheUpdateStrategy(elasticacheConfig, foundCache)
	if ec != nil {
		if _, err = cacheSvc.ModifyReplicationGroup(ec); err != nil {
			errMsg := "failed to modify elasticache cluster"
			return nil, croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
		}
		return nil, croType.StatusMessage(fmt.Sprintf("changes detected, modifyDBInstance() in progress, current aws elasticache status is %s", *foundCache.Status)), nil
	}

	// add tags to cache nodes
	cacheInstance := *foundCache.NodeGroups[0]
	if *cacheInstance.Status != "available" {
		return nil, croType.StatusMessage(fmt.Sprintf("cache node status not available, current status:  %s", *foundCache.Status)), nil
	}

	for _, cache := range cacheInstance.NodeGroupMembers {
		msg, err := p.TagElasticacheNode(ctx, cacheSvc, stsSvc, r, *stratCfg, cache)
		if err != nil {
			errMsg := fmt.Sprintf("failed to add tags to elasticache: %s", msg)
			return nil, types.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
		}
	}

	// return secret information
	primaryEndpoint := foundCache.NodeGroups[0].PrimaryEndpoint
	return &providers.RedisCluster{DeploymentDetails: &providers.RedisDeploymentDetails{
		URI:  *primaryEndpoint.Address,
		Port: *primaryEndpoint.Port,
	}}, croType.StatusMessage(fmt.Sprintf("successfully created and tagged, aws elasticache status is %s", *foundCache.Status)), nil
}

// Add Tags to AWS Elasticache
func (p *RedisProvider) TagElasticacheNode(ctx context.Context, cacheSvc elasticacheiface.ElastiCacheAPI, stsSvc stsiface.STSAPI, r *v1alpha1.Redis, stratCfg StrategyConfig, cache *elasticache.NodeGroupMember) (types.StatusMessage, error) {
	logrus.Info("creating or updating tags on elasticache nodes and snapshots")

	// check the node to make sure it is available before applying the tag
	// this is needed as the cluster may be available while a node is not
	cacheClusterOutput, err := cacheSvc.DescribeCacheClusters(&elasticache.DescribeCacheClustersInput{
		CacheClusterId: cache.CacheClusterId,
	})
	if err != nil {
		errMsg := "failed to get cache cluster output"
		return types.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}
	clusterStatus := *cacheClusterOutput.CacheClusters[0].CacheClusterStatus
	if clusterStatus != "available" {
		errMsg := fmt.Sprintf("%s status is %s, skipping adding tags", *cache.CacheClusterId, clusterStatus)
		return types.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// get account identity
	identityInput := &sts.GetCallerIdentityInput{}
	id, err := stsSvc.GetCallerIdentity(identityInput)
	if err != nil {
		errMsg := "failed to get account identity"
		return types.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// trim availability zone to return cache region
	region := (*cache.PreferredAvailabilityZone)[:len(*cache.PreferredAvailabilityZone)-1]

	// build cluster arn
	// need arn in the following format arn:aws:elasticache:us-east-1:1234567890:cluster:my-mem-cluster
	arn := fmt.Sprintf("arn:aws:elasticache:%s:%s:cluster:%s", region, *id.Account, *cache.CacheClusterId)

	// set the tag values that will always be added
	organizationTag := resources.GetOrganizationTag()
	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		errMsg := "failed to get cluster id"
		return croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
	}
	cacheTags := []*elasticache.Tag{
		{
			Key:   aws.String(organizationTag + "clusterID"),
			Value: aws.String(clusterID),
		},
		{
			Key:   aws.String(organizationTag + "resource-type"),
			Value: aws.String(r.Spec.Type),
		},
		{
			Key:   aws.String(organizationTag + "resource-name"),
			Value: aws.String(r.Name),
		},
	}

	// check is product name exists on cr
	if r.ObjectMeta.Labels["productName"] != "" {
		productTag := &elasticache.Tag{
			Key:   aws.String(organizationTag + "product-name"),
			Value: aws.String(r.ObjectMeta.Labels["productName"]),
		}
		cacheTags = append(cacheTags, productTag)
	}

	// add tags
	_, err = cacheSvc.AddTagsToResource(&elasticache.AddTagsToResourceInput{
		ResourceName: aws.String(arn),
		Tags:         cacheTags,
	})
	if err != nil {
		msg := "failed to add tags to aws elasticache :"
		return types.StatusMessage(msg), err
	}

	// if snapshots exist add tags to them
	inputDescribe := &elasticache.DescribeSnapshotsInput{
		CacheClusterId: aws.String(*cache.CacheClusterId),
	}

	// loop snapshots adding tags per found snapshot
	snapshotList, _ := cacheSvc.DescribeSnapshots(inputDescribe)
	if snapshotList.Snapshots != nil {
		for _, snapshot := range snapshotList.Snapshots {
			snapshotArn := fmt.Sprintf("arn:aws:elasticache:%s:%s:snapshot:%s", region, *id.Account, *snapshot.SnapshotName)
			logrus.Infof("Adding operator tags to snapshot : %s", *snapshot.SnapshotName)
			snapshotInput := &elasticache.AddTagsToResourceInput{
				ResourceName: aws.String(snapshotArn),
				Tags:         cacheTags,
			}
			_, err = cacheSvc.AddTagsToResource(snapshotInput)
			if err != nil {
				msg := "failed to add tags to aws elasticache snapshot"
				return types.StatusMessage(msg), err
			}
		}
	}

	logrus.Infof("successfully created or updated tags to elasticache node %s", *cache.CacheClusterId)
	return "successfully created and tagged", nil
}

// DeleteStorage Delete elasticache replication group
func (p *RedisProvider) DeleteRedis(ctx context.Context, r *v1alpha1.Redis) (croType.StatusMessage, error) {
	// resolve elasticache information for elasticache created by provider
	p.Logger.Info("getting cluster id from infrastructure for redis naming")
	elasticacheCreateConfig, elasticacheDeleteConfig, stratCfg, err := p.getElasticacheConfig(ctx, r)
	if err != nil {
		errMsg := fmt.Sprintf("failed to retrieve aws elasticache config for instance %s", r.Name)
		return croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
	}

	// get provider aws creds so the elasticache cluster can be deleted
	providerCreds, err := p.CredentialManager.ReconcileProviderCredentials(ctx, r.Namespace)
	if err != nil {
		errMsg := "failed to reconcile aws provider credentials"
		return croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// setup aws elasticache cluster sdk session
	cacheSvc, _ := createAWSService(stratCfg, providerCreds)

	// delete the elasticache cluster
	return p.deleteElasticacheCluster(cacheSvc, elasticacheCreateConfig, elasticacheDeleteConfig, ctx, r)
}

func (p *RedisProvider) deleteElasticacheCluster(cacheSvc elasticacheiface.ElastiCacheAPI, elasticacheCreateConfig *elasticache.CreateReplicationGroupInput, elasticacheDeleteConfig *elasticache.DeleteReplicationGroupInput, ctx context.Context, r *v1alpha1.Redis) (croType.StatusMessage, error) {
	// the aws access key can sometimes still not be registered in aws on first try, so loop
	rgs, err := getReplicationGroups(cacheSvc)
	if err != nil {
		return "error getting replication groups", err
	}

	// check and verify delete config
	if err := p.buildElasticacheDeleteConfig(ctx, *r, elasticacheCreateConfig, elasticacheDeleteConfig); err != nil {
		errMsg := "failed to verify aws rds instance configuration"
		return croType.StatusMessage(errMsg), errorUtil.Wrap(err, errMsg)
	}

	// check if the cluster has already been deleted
	var foundCache *elasticache.ReplicationGroup
	for _, c := range rgs {
		if *c.ReplicationGroupId == *elasticacheCreateConfig.ReplicationGroupId {
			foundCache = c
			break
		}
	}

	// check if replication group does not exist and delete finalizer
	if foundCache == nil {
		// remove the finalizer added by the provider
		resources.RemoveFinalizer(&r.ObjectMeta, DefaultFinalizer)
		if err := p.Client.Update(ctx, r); err != nil {
			errMsg := "failed to update instance as part of finalizer reconcile"
			return croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
		}
		return croType.StatusEmpty, nil
	}

	// set status metric
	if err := p.exposeRedisMetrics(ctx, r, foundCache); err != nil {
		return "failed to set metric", err
	}

	// if status is not available return
	if *foundCache.Status != "available" {
		return croType.StatusMessage(fmt.Sprintf("delete detected, deleteReplicationGroup() in progress, current aws elasticache status is %s", *foundCache.Status)), nil
	}

	// Delete the PrometheusRule alert which watches the availability of this ElastiCache instance we provisioned
	err = p.DeleteElastiCacheAvailabilityAlert(ctx, r.Namespace, *foundCache.ReplicationGroupId)
	if err != nil {
		errMsg := fmt.Sprintf("failed to delete elasticache alert : %s", err)
		return croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
	}

	// delete elasticache cluster
	_, err = cacheSvc.DeleteReplicationGroup(elasticacheDeleteConfig)
	elasticacheErr, isAwsErr := err.(awserr.Error)
	if err != nil && (!isAwsErr || elasticacheErr.Code() != elasticache.ErrCodeReplicationGroupNotFoundFault) {
		errMsg := fmt.Sprintf("failed to delete elasticache cluster : %s", err)
		return croType.StatusMessage(errMsg), errorUtil.Wrapf(err, errMsg)
	}
	return "delete detected, deleteReplicationGroup started", nil
}

// poll for replication groups
func getReplicationGroups(cacheSvc elasticacheiface.ElastiCacheAPI) ([]*elasticache.ReplicationGroup, error) {
	var rgs []*elasticache.ReplicationGroup
	err := wait.PollImmediate(time.Second*5, time.Minute*5, func() (done bool, err error) {
		listOutput, err := cacheSvc.DescribeReplicationGroups(&elasticache.DescribeReplicationGroupsInput{})
		if err != nil {
			return false, nil
		}
		rgs = listOutput.ReplicationGroups
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return rgs, nil
}

// getElasticacheConfig retrieves the elasticache config from the cloud-resources-aws-strategies configmap
func (p *RedisProvider) getElasticacheConfig(ctx context.Context, r *v1alpha1.Redis) (*elasticache.CreateReplicationGroupInput, *elasticache.DeleteReplicationGroupInput, *StrategyConfig, error) {
	stratCfg, err := p.ConfigManager.ReadStorageStrategy(ctx, providers.RedisResourceType, r.Spec.Tier)
	if err != nil {
		return nil, nil, nil, errorUtil.Wrap(err, "failed to read aws strategy config")
	}
	if stratCfg.Region == "" {
		stratCfg.Region = DefaultRegion
	}

	// unmarshal the elasticache cluster config
	elasticacheCreateConfig := &elasticache.CreateReplicationGroupInput{}
	if err := json.Unmarshal(stratCfg.CreateStrategy, elasticacheCreateConfig); err != nil {
		return nil, nil, nil, errorUtil.Wrap(err, "failed to unmarshal aws elasticache cluster configuration")
	}

	elasticacheDeleteConfig := &elasticache.DeleteReplicationGroupInput{}
	if err := json.Unmarshal(stratCfg.DeleteStrategy, elasticacheDeleteConfig); err != nil {
		return nil, nil, nil, errorUtil.Wrap(err, "failed to unmarshal aws elasticache cluster configuration")
	}
	return elasticacheCreateConfig, elasticacheDeleteConfig, stratCfg, nil
}

// checks found config vs user strategy for changes, if found returns a modify replication group
func buildElasticacheUpdateStrategy(elasticacheConfig *elasticache.CreateReplicationGroupInput, foundConfig *elasticache.ReplicationGroup) *elasticache.ModifyReplicationGroupInput {
	updateFound := false

	ec := &elasticache.ModifyReplicationGroupInput{}
	ec.ReplicationGroupId = foundConfig.ReplicationGroupId

	if *elasticacheConfig.CacheNodeType != *foundConfig.CacheNodeType {
		ec.CacheNodeType = elasticacheConfig.CacheNodeType
		updateFound = true
	}
	if *elasticacheConfig.SnapshotRetentionLimit != *foundConfig.SnapshotRetentionLimit {
		ec.SnapshotRetentionLimit = elasticacheConfig.SnapshotRetentionLimit
		updateFound = true
	}
	if updateFound {
		return ec
	}
	return nil
}

// verifyRedisConfig checks elasticache config, if none exist sets values to default
func (p *RedisProvider) buildElasticacheCreateStrategy(ctx context.Context, r *v1alpha1.Redis, elasticacheConfig *elasticache.CreateReplicationGroupInput) error {

	elasticacheConfig.AutomaticFailoverEnabled = aws.Bool(true)
	elasticacheConfig.Engine = aws.String("redis")

	if elasticacheConfig.CacheNodeType == nil {
		elasticacheConfig.CacheNodeType = aws.String(defaultCacheNodeType)
	}
	if elasticacheConfig.ReplicationGroupDescription == nil {
		elasticacheConfig.ReplicationGroupDescription = aws.String(defaultDescription)
	}
	if elasticacheConfig.EngineVersion == nil {
		elasticacheConfig.EngineVersion = aws.String(defaultEngineVersion)
	}
	if elasticacheConfig.NumCacheClusters == nil {
		elasticacheConfig.NumCacheClusters = aws.Int64(defaultNumCacheClusters)
	}
	if elasticacheConfig.SnapshotRetentionLimit == nil {
		elasticacheConfig.SnapshotRetentionLimit = aws.Int64(defaultSnapshotRetention)
	}
	cacheName, err := BuildInfraNameFromObject(ctx, p.Client, r.ObjectMeta, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrapf(err, "failed to retrieve elasticache config")
	}
	if elasticacheConfig.ReplicationGroupId == nil {
		elasticacheConfig.ReplicationGroupId = aws.String(cacheName)
	}
	return nil
}

// buildElasticacheDeleteConfig checks redis config, if none exists sets values to defaults
func (p *RedisProvider) buildElasticacheDeleteConfig(ctx context.Context, r v1alpha1.Redis, elasticacheCreateConfig *elasticache.CreateReplicationGroupInput, elasticacheDeleteConfig *elasticache.DeleteReplicationGroupInput) error {
	cacheName, err := BuildInfraNameFromObject(ctx, p.Client, r.ObjectMeta, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrapf(err, "failed to retrieve elasticache config")
	}
	if elasticacheDeleteConfig.ReplicationGroupId == nil {
		if elasticacheCreateConfig.ReplicationGroupId == nil {
			elasticacheCreateConfig.ReplicationGroupId = aws.String(cacheName)
		}
		elasticacheDeleteConfig.ReplicationGroupId = elasticacheCreateConfig.ReplicationGroupId
	}
	if elasticacheDeleteConfig.RetainPrimaryCluster == nil {
		elasticacheDeleteConfig.RetainPrimaryCluster = aws.Bool(false)
	}
	snapshotIdentifier, err := buildTimestampedInfraNameFromObject(ctx, p.Client, r.ObjectMeta, DefaultAwsIdentifierLength)
	if err != nil {
		return errorUtil.Wrapf(err, "failed to retrieve rds config")
	}
	if elasticacheDeleteConfig.FinalSnapshotIdentifier != nil && *elasticacheDeleteConfig.FinalSnapshotIdentifier == "" {
		elasticacheDeleteConfig.FinalSnapshotIdentifier = aws.String(snapshotIdentifier)
	}
	return nil
}

// returns generic labels to be added to every metric
func buildRedisGenericMetricLabels(r *v1alpha1.Redis, cache *elasticache.ReplicationGroup, clusterID string) map[string]string {
	labels := map[string]string{}
	labels["clusterID"] = clusterID
	labels["resourceID"] = r.Name
	labels["namespace"] = r.Namespace
	labels["instanceID"] = *cache.ReplicationGroupId
	return labels
}

// adds extra information to labels around resource
func buildRedisInfoMetricLables(r *v1alpha1.Redis, cache *elasticache.ReplicationGroup, clusterID string) map[string]string {
	labels := buildRedisGenericMetricLabels(r, cache, clusterID)
	labels["status"] = *cache.Status
	return labels
}

// used to expose an available and information metrics during reconcile
func (p *RedisProvider) exposeRedisMetrics(ctx context.Context, cr *v1alpha1.Redis, instance *elasticache.ReplicationGroup) error {
	logrus.Info("setting redis information metric")
	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		return errorUtil.Wrap(err, "failed to get cluster id")
	}

	// build metric labels
	infoLabels := buildRedisInfoMetricLables(cr, instance, clusterID)
	genericLabels := buildRedisGenericMetricLabels(cr, instance, clusterID)

	// set status gauge
	if err := resources.SetMetricCurrentTime(defaultRedisInfoMetricName, infoLabels); err != nil {
		return err
	}

	// set available metric
	if *instance.Status != "available" {
		if err := resources.SetMetric(defaultRedisAvailMetricName, genericLabels, 0); err != nil {
			return err
		}
		return nil
	}
	if err := resources.SetMetric(defaultRedisAvailMetricName, genericLabels, 1); err != nil {
		return err
	}

	return nil
}

// sets maintenance metric
func (p *RedisProvider) setRedisServiceMaintenanceMetric(ctx context.Context, cr *v1alpha1.Redis, cacheSvc elasticacheiface.ElastiCacheAPI, instance *elasticache.ReplicationGroup) error {
	// info about the elasticache cluster to be created
	logrus.Info("checking for pending redis service updates")
	clusterID, err := resources.GetClusterID(ctx, p.Client)
	if err != nil {
		return errorUtil.Wrap(err, "failed to get cluster id")
	}

	// Retrieve service maintenance updates, create and export Prometheus metrics
	output, err := cacheSvc.DescribeServiceUpdates(&elasticache.DescribeServiceUpdatesInput{})
	if err != nil {
		return errorUtil.Wrap(err, "elasticache serviceupdates error")
	}

	logrus.Info(fmt.Sprintf("there are elasticache service updates: %d available", len(output.ServiceUpdates)))
	for _, su := range output.ServiceUpdates {
		metricLabels := map[string]string{}
		metricLabels["clusterID"] = clusterID

		metricLabels["AutoUpdateAfterRecommendedApplyByDate"] = strconv.FormatBool(*su.AutoUpdateAfterRecommendedApplyByDate)
		metricLabels["Engine"] = *su.Engine
		metricLabels["EstimatedUpdateTime"] = *su.EstimatedUpdateTime
		metricLabels["ServiceUpdateDescription"] = *su.ServiceUpdateDescription
		metricLabels["ServiceUpdateEndDate"] = strconv.FormatInt((*su.ServiceUpdateEndDate).Unix(), 10)
		metricLabels["ServiceUpdateName"] = *su.ServiceUpdateName
		metricLabels["ServiceUpdateRecommendedApplyByDate"] = strconv.FormatInt((*su.ServiceUpdateRecommendedApplyByDate).Unix(), 10)
		metricLabels["ServiceUpdateReleaseDate"] = strconv.FormatInt((*su.ServiceUpdateReleaseDate).Unix(), 10)
		metricLabels["ServiceUpdateSeverity"] = *su.ServiceUpdateSeverity
		metricLabels["ServiceUpdateStatus"] = *su.ServiceUpdateStatus
		metricLabels["ServiceUpdateType"] = *su.ServiceUpdateType

		metricEpochTimestamp := (*su.ServiceUpdateRecommendedApplyByDate).Unix()

		err = croResources.SetMetric(defaultRedisMaintenanceMetricName, metricLabels, float64(metricEpochTimestamp))
		if err != nil {
			msg := fmt.Sprintf("exception calling SetMetric with metricName: %s", defaultRedisMaintenanceMetricName)
			return errorUtil.Wrap(err, msg)
		}
	}
	return nil
}

// CreateElastiCacheAvailabilityAlert Call this when we create the ElastiCache instance to create an
// alert to watch for the availability of the instance
func (p *RedisProvider) CreateElastiCacheAvailabilityAlert(ctx context.Context, r *v1alpha1.Redis, instanceID string, clusterID string) error {
	alertRuleName := fmt.Sprintf("cro-aws-elasticache-%s", instanceID)
	alertExp := intstr.FromString(
		fmt.Sprintf("absent(cro_aws_elasticache_available{namespace='%s',instanceID='%s',clusterID='%s',resourceID='%s'} == 1)",
			r.Namespace, instanceID, clusterID, r.Name),
	)

	pr, err := croResources.CreatePrometheusRule(r.Namespace, alertRuleName, alertExp)
	if err != nil {
		return err
	}

	// Unless it already exists, call the kubernetes api and create this PrometheusRule
	// Replace this with CreateOrUpdate if we can figure it out
	err = p.Client.Create(ctx, pr)
	if err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return errorUtil.Wrap(err, fmt.Sprintf("exception calling Create metricName: %s", alertRuleName))
		}
	}
	p.Logger.Info(fmt.Sprintf("PrometheusRule: %s reconcilced successfully.", pr.Name))
	return nil
}

// DeleteElastiCacheAvailabilityAlert We call this when we delete an ElastiCache instance,
// it removes the prometheusrule alert which watches for the availability of the instance.
func (p *RedisProvider) DeleteElastiCacheAvailabilityAlert(ctx context.Context, namespace string, instanceID string) error {
	// query the kubernetes api to find the object we're looking for
	alertRuleName := fmt.Sprintf("cro-aws-elasticache-%s", instanceID)

	pr := &prometheusv1.PrometheusRule{}
	selector := client.ObjectKey{
		Namespace: namespace,
		Name:      alertRuleName,
	}

	if err := p.Client.Get(ctx, selector, pr); err != nil {
		msg := fmt.Sprintf("exception calling DeleteElastiCacheAvailabilityAlert: %s", alertRuleName)
		return errorUtil.Wrap(err, msg)
	}

	// call delete on that object
	if err := p.Client.Delete(ctx, pr); err != nil {
		msg := fmt.Sprintf("exception calling DeleteElastiCacheAvailabilityAlert: %s", alertRuleName)
		return errorUtil.Wrap(err, msg)
	}
	p.Logger.Info(fmt.Sprintf("PrometheusRule: %s deleted.", pr.Name))
	return nil
}
