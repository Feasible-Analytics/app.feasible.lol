//
// policy_test.go
// Tests for rendering and validating the replica lifecycle policy.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package replica

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestRenderAndValidateLifecyclePolicy proves the shipped template covers the
// complete shard prefix and passes the same check production runs.
func TestRenderAndValidateLifecyclePolicy(t *testing.T) {
	body, err := Render("s3://replicas/shard-01")
	if err != nil {
		t.Fatal(err)
	}

	text := string(body)
	for _, want := range []string{PolicyID, `"Prefix": "shard-01/"`, `"Days": 2`, `"NoncurrentDays": 2`} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered policy is missing %q:\n%s", want, text)
		}
	}

	if err := Validate("s3://replicas/shard-01", body, []byte(`{}`), []byte(`{}`)); err != nil {
		t.Fatalf("rendered policy did not validate: %v", err)
	}
}

// TestAttestationIsFreshAndLocationBound proves provider responses cannot be
// replayed indefinitely or mixed with a different bucket or shard prefix.
func TestAttestationIsFreshAndLocationBound(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	replicaURL := "s3://replicas/shard-01"
	policy, err := Render(replicaURL)
	if err != nil {
		t.Fatal(err)
	}
	evidence := Attestation{
		Version: AttestationVersion, FetchedAt: now, ReplicaURL: replicaURL,
		Bucket: "replicas", Prefix: "shard-01/",
		BucketLocation: json.RawMessage(`{"LocationConstraint":"us-west-2"}`),
		Lifecycle:      policy, Versioning: json.RawMessage(`{}`), ObjectLock: json.RawMessage(`{}`),
	}
	encode := func(value Attestation) []byte {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	if err := ValidateAttestation(replicaURL, encode(evidence), now); err != nil {
		t.Fatalf("fresh bound evidence failed: %v", err)
	}

	stale := evidence
	stale.FetchedAt = now.Add(-AttestationMaxAge - time.Second)
	if err := ValidateAttestation(replicaURL, encode(stale), now); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("stale evidence error = %v", err)
	}
	wrong := evidence
	wrong.Prefix = "shard-02/"
	if err := ValidateAttestation(replicaURL, encode(wrong), now); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("wrong-prefix evidence error = %v", err)
	}
	missingLocation := evidence
	missingLocation.BucketLocation = json.RawMessage(`{}`)
	if err := ValidateAttestation(replicaURL, encode(missingLocation), now); err == nil || !strings.Contains(err.Error(), "GetBucketLocation") {
		t.Fatalf("missing location evidence error = %v", err)
	}
}

// TestAccessTemplateKeepsDeletionOutOfApplicationCredentials checks the
// shipped role split: only the lifecycle owner can change retention, the
// deployment checker is read-only, and no principal can directly delete a
// replica object.
func TestAccessTemplateKeepsDeletionOutOfApplicationCredentials(t *testing.T) {
	body, err := os.ReadFile("../../ops/s3/replica-access-v1.jsonc")
	if err != nil {
		t.Fatal(err)
	}

	text := string(body)
	for _, want := range []string{
		`"replicator"`, `"deployment_checker"`, `"lifecycle_owner"`,
		`s3:GetObject`, `s3:PutObject`, `s3:GetLifecycleConfiguration`,
		`s3:GetBucketVersioning`, `s3:GetBucketObjectLockConfiguration`,
		`s3:GetBucketLocation`,
		`s3:PutLifecycleConfiguration`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("access template is missing %q", want)
		}
	}
	if strings.Contains(text, `"s3:DeleteObject"`) {
		t.Fatal("application credentials can directly delete replica objects")
	}
	allowed := map[string]bool{
		"s3:prefix":     true,
		"s3:ListBucket": true, "s3:GetBucketLocation": true, "s3:GetObject": true,
		"s3:PutObject": true, "s3:GetBucketVersioning": true,
		"s3:GetLifecycleConfiguration": true, "s3:GetBucketObjectLockConfiguration": true,
		"s3:PutLifecycleConfiguration": true,
	}
	for _, match := range regexp.MustCompile(`"(s3:[A-Za-z]+)"`).FindAllStringSubmatch(text, -1) {
		if !allowed[match[1]] {
			t.Errorf("access template grants unreviewed action %s", match[1])
		}
	}
}

// TestAttestationScriptPublishesOneCompleteGeneration checks the deployment
// fetch cannot expose three independently renamed, mixable provider snapshots.
func TestAttestationScriptPublishesOneCompleteGeneration(t *testing.T) {
	body, err := os.ReadFile("../../scripts/check-replica-lifecycle.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"get-bucket-location", "FEASIBLE_LITESTREAM_ATTESTATION", "fetched_at", "bucket_location", "replica_url"} {
		if !strings.Contains(text, want) {
			t.Errorf("attestation script is missing %q", want)
		}
	}
	if strings.Count(text, "\nmv ") != 1 {
		t.Fatalf("attestation publication has %d rename steps, want one", strings.Count(text, "\nmv "))
	}
	for _, old := range []string{"FEASIBLE_LITESTREAM_LIFECYCLE_POLICY", "FEASIBLE_LITESTREAM_BUCKET_VERSIONING", "FEASIBLE_LITESTREAM_OBJECT_LOCK"} {
		if strings.Contains(text, old) {
			t.Errorf("attestation script still publishes mixable variable %s", old)
		}
	}
}

// TestValidateLifecyclePolicyFailsClosed walks the provider states that can
// retain a deleted account prefix or an old control snapshot past the bound.
func TestValidateLifecyclePolicyFailsClosed(t *testing.T) {
	valid, err := Render("s3://replicas/shard-01")
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		policy     []byte
		versioning []byte
		objectLock []byte
		want       string
	}{
		"missing rule": {[]byte(`{"Rules":[]}`), []byte(`{}`), []byte(`{}`), "absent"},
		"wrong prefix": {[]byte(strings.Replace(string(valid), "shard-01/", "shard-02/", 1)), []byte(`{}`), []byte(`{}`), "absent"},
		"too long":     {[]byte(strings.Replace(string(valid), `"Days": 2`, `"Days": 3`, 1)), []byte(`{}`), []byte(`{}`), "current-object"},
		"versioned":    {valid, []byte(`{"Status":"Enabled"}`), []byte(`{}`), "versioning"},
		"suspended":    {valid, []byte(`{"Status":"Suspended"}`), []byte(`{}`), "versioning"},
		"object lock":  {valid, []byte(`{}`), []byte(`{"ObjectLockConfiguration":{"ObjectLockEnabled":"Enabled"}}`), "Object Lock"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := Validate("s3://replicas/shard-01", test.policy, test.versioning, test.objectLock)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want text %q", err, test.want)
			}
		})
	}
}

// TestParseLocationRequiresAnIsolatedPrefix prevents a lifecycle policy from
// accidentally targeting an entire shared bucket.
func TestParseLocationRequiresAnIsolatedPrefix(t *testing.T) {
	for _, raw := range []string{"", "https://bucket/shard-01", "s3://bucket"} {
		if _, err := ParseLocation(raw); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
}
