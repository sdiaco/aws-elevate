package release

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// buckets maps each region where aws-elevate is published to its release bucket
// (contract/release-buckets.json). The bucket is never derived from the
// region name: in a region without a release someone else could own that name and serve
// their own template and code.
var buckets = map[string]string{
	"ca-central-1": "aws-elevate-releases-ca-central-1",
	"eu-central-1": "aws-elevate-releases-eu-central-1",
	"eu-north-1":   "aws-elevate-releases-eu-north-1",
	"eu-south-1":   "aws-elevate-releases-eu-south-1",
	"eu-west-1":    "aws-elevate-releases-eu-west-1",
	"eu-west-2":    "aws-elevate-releases-eu-west-2",
	"eu-west-3":    "aws-elevate-releases-eu-west-3",
	"sa-east-1":    "aws-elevate-releases-sa-east-1",
	"us-east-1":    "aws-elevate-releases-us-east-1",
	"us-east-2":    "aws-elevate-releases-us-east-2",
	"us-west-1":    "aws-elevate-releases-us-west-1",
	"us-west-2":    "aws-elevate-releases-us-west-2",
}

// TemplateURL is the infrastructure template of version v in region, or an error when
// aws-elevate is not published there.
func TemplateURL(region, v string) (string, error) {
	bucket, ok := buckets[region]
	if !ok {
		return "", fmt.Errorf("aws-elevate is not published in %s; regions with a release: %s",
			region, strings.Join(slices.Sorted(maps.Keys(buckets)), ", "))
	}
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/v%s/elevate.yaml", bucket, region, v), nil
}

// Bucket is the release bucket of region, empty where aws-elevate is not published.
func Bucket(region string) string { return buckets[region] }
