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
	"eu-west-1": "aws-elevate-releases-eu-west-1",
	"us-east-1": "aws-elevate-releases-us-east-1",
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
