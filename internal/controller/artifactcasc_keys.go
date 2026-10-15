package controller

// JCasC key paths for the artifact-manager-s3 CasC fragment (task 0.5's
// compatibility spike, review-log.md "Task 0.5"). Tasks 5.2, 5.3, and 5.3b
// build the fragment from these paths. The keys match the pinned plugin
// version's exported schema, not the design draft: the credential and the
// S3 settings live under the `aws` root, never under `unclassified`.

// ArtifactManagerFactoryPath is the key path of the artifact manager
// factory list. The export writes the input form `provider: {s3: {}}` as
// `provider: "s3"`.
const ArtifactManagerFactoryPath = "unclassified.artifactManager.artifactManagerFactories[].jclouds.provider"

// ArtifactManagerS3PrefixKey is the per-cell object prefix key. Contract D
// fixes its value to `artifacts/<cluster>/<namespace>/<cell>/`.
const ArtifactManagerS3PrefixKey = "aws.s3.prefix"

// Remaining key paths the fragment writes under the `aws` root.
const (
	ArtifactManagerCredentialsIDKey    = "aws.awsCredentials.credentialsId"
	ArtifactManagerRegionKey           = "aws.awsCredentials.region"
	ArtifactManagerContainerKey        = "aws.s3.container"
	ArtifactManagerCustomEndpointKey   = "aws.s3.customEndpoint"
	ArtifactManagerUsePathStyleURLKey  = "aws.s3.usePathStyleUrl"
	ArtifactManagerUseHTTPKey          = "aws.s3.useHttp"
	ArtifactManagerDisableSessionToken = "aws.s3.disableSessionToken"
	ArtifactManagerCustomSigningRegion = "aws.s3.customSigningRegion"
	ArtifactManagerUseTransferAccel    = "aws.s3.useTransferAcceleration"
)

// ArtifactManagerJCasCKeys names every JCasC key path task 0.5's export
// named. Tasks 5.2, 5.3, and 5.3b read this list to build the fragment.
var ArtifactManagerJCasCKeys = []string{
	ArtifactManagerFactoryPath,
	ArtifactManagerCredentialsIDKey,
	ArtifactManagerRegionKey,
	ArtifactManagerContainerKey,
	ArtifactManagerS3PrefixKey,
	ArtifactManagerCustomEndpointKey,
	ArtifactManagerUsePathStyleURLKey,
	ArtifactManagerUseHTTPKey,
	ArtifactManagerDisableSessionToken,
	ArtifactManagerCustomSigningRegion,
	ArtifactManagerUseTransferAccel,
}
