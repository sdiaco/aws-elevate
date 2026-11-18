package release

import (
	"encoding/json"
	"maps"
	"os"
	"strings"
	"testing"
)

func TestBucketsMatchTheContract(t *testing.T) {
	raw, err := os.ReadFile("../../../contract/release-buckets.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(buckets, want) {
		t.Fatalf("buckets %v, contract %v", buckets, want)
	}
}

func TestTemplateURLUsesTheMappedBucket(t *testing.T) {
	got, err := TemplateURL("us-east-1", "0.2.0")
	if err != nil || got != "https://elevate-releases-us-east-1.s3.us-east-1.amazonaws.com/v0.2.0/elevate.yaml" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestTemplateURLRefusesARegionWithoutRelease(t *testing.T) {
	_, err := TemplateURL("ap-south-1", "0.2.0")
	if err == nil || !strings.Contains(err.Error(), "not published in ap-south-1") || !strings.Contains(err.Error(), "eu-west-1, us-east-1") {
		t.Fatalf("%v", err)
	}
}
