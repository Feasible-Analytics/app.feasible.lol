//
// policy.go
// Rendering and validating the object-storage lifecycle that bounds replicas.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package replica defines the provider-side retention control for encrypted
// database replicas. Litestream retention cannot remove an account prefix once
// that database leaves its generated configuration, so the bucket lifecycle is
// the durable deletion mechanism and is validated independently.
package replica

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// PolicyID versions the lifecycle contract in provider configuration and in
	// audit output. Changing its semantics requires a new id rather than silently
	// changing what an existing provider rule is believed to mean.
	PolicyID = "feasible-replica-expiration-v1"

	// ExpirationDays is two because S3 rounds a day-based expiration up to the
	// following midnight UTC. Two configured days therefore makes an object
	// eligible after at least 48 and no more than 72 hours.
	ExpirationDays = 2

	// AttestationVersion identifies the atomic provider-evidence bundle format.
	AttestationVersion = "feasible-replica-attestation-v1"

	// AttestationMaxAge prevents a once-valid bucket snapshot from authorizing
	// an unrelated later deployment after provider controls have changed.
	AttestationMaxAge = 15 * time.Minute
)

// Attestation is one atomic provider snapshot, bound to a replica bucket and
// prefix. Raw provider responses stay together so files from different fetches
// cannot be mixed into apparently valid evidence.
type Attestation struct {
	Version        string          `json:"version"`
	FetchedAt      time.Time       `json:"fetched_at"`
	ReplicaURL     string          `json:"replica_url"`
	Bucket         string          `json:"bucket"`
	Prefix         string          `json:"prefix"`
	BucketLocation json.RawMessage `json:"bucket_location"`
	Lifecycle      json.RawMessage `json:"lifecycle"`
	Versioning     json.RawMessage `json:"versioning"`
	ObjectLock     json.RawMessage `json:"object_lock"`
}

// LifecycleConfiguration is the S3-compatible lifecycle document returned by
// GetBucketLifecycleConfiguration and accepted by PutBucketLifecycleConfiguration.
type LifecycleConfiguration struct {
	Rules []Rule `json:"Rules"`
}

// Rule is the subset of an S3 lifecycle rule needed to prove replica expiry.
type Rule struct {
	ID                             string                      `json:"ID"`
	Status                         string                      `json:"Status"`
	Filter                         Filter                      `json:"Filter"`
	Prefix                         string                      `json:"Prefix,omitempty"`
	Expiration                     Expiration                  `json:"Expiration"`
	NoncurrentVersionExpiration    NoncurrentVersionExpiration `json:"NoncurrentVersionExpiration"`
	AbortIncompleteMultipartUpload AbortIncompleteMultipart    `json:"AbortIncompleteMultipartUpload"`
}

// Filter restricts the lifecycle rule to one shard's complete replica prefix.
type Filter struct {
	Prefix string `json:"Prefix"`
}

// Expiration controls when current objects become eligible for removal.
type Expiration struct {
	Days int `json:"Days"`
}

// NoncurrentVersionExpiration removes historical versions without retaining a
// fixed number that could outlive the public bound.
type NoncurrentVersionExpiration struct {
	NoncurrentDays          int `json:"NoncurrentDays"`
	NewerNoncurrentVersions int `json:"NewerNoncurrentVersions,omitempty"`
}

// AbortIncompleteMultipart removes uploads that never became usable replicas.
type AbortIncompleteMultipart struct {
	DaysAfterInitiation int `json:"DaysAfterInitiation"`
}

// BucketVersioning is the provider response from GetBucketVersioning.
type BucketVersioning struct {
	Status string `json:"Status"`
}

// ObjectLockConfiguration is the provider response from
// GetObjectLockConfiguration.
type ObjectLockConfiguration struct {
	ObjectLockEnabled string `json:"ObjectLockEnabled"`
}

// ObjectLockResponse is the JSON envelope emitted by the S3 API and AWS CLI.
// Keeping the envelope in the type prevents a valid enabled response from
// silently decoding as an empty, apparently unlocked configuration.
type ObjectLockResponse struct {
	Configuration ObjectLockConfiguration `json:"ObjectLockConfiguration"`
}

// Location is the bucket and key prefix parsed from one S3 replica URL.
type Location struct {
	Bucket string
	Prefix string
}

// ParseLocation validates an S3 replica URL and returns the exact provider
// location the lifecycle rule must cover.
func ParseLocation(replicaURL string) (Location, error) {
	parsed, err := url.Parse(strings.TrimSpace(replicaURL))
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" {
		return Location{}, fmt.Errorf("replica lifecycle: %q is not an s3://bucket/prefix URL", replicaURL)
	}

	prefix := strings.Trim(parsed.Path, "/")
	if prefix == "" {
		return Location{}, fmt.Errorf("replica lifecycle: %q needs a shard prefix below the bucket", replicaURL)
	}

	return Location{Bucket: parsed.Host, Prefix: prefix + "/"}, nil
}

// Render returns the canonical, versioned lifecycle policy for one replica
// prefix. The rule covers control and every account key below that prefix,
// including keys for a database that Litestream no longer configures.
func Render(replicaURL string) ([]byte, error) {
	location, err := ParseLocation(replicaURL)
	if err != nil {
		return nil, err
	}

	policy := LifecycleConfiguration{Rules: []Rule{{
		ID:     PolicyID,
		Status: "Enabled",
		Filter: Filter{Prefix: location.Prefix},
		Expiration: Expiration{
			Days: ExpirationDays,
		},
		NoncurrentVersionExpiration: NoncurrentVersionExpiration{
			NoncurrentDays: ExpirationDays,
		},
		AbortIncompleteMultipartUpload: AbortIncompleteMultipart{
			DaysAfterInitiation: 1,
		},
	}}}

	body, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("replica lifecycle: render policy: %w", err)
	}

	return append(body, '\n'), nil
}

// Validate checks provider exports rather than a desired local template. It
// rejects versioning and Object Lock because either can retain a recoverable
// version after current-object expiration, outside the day-based bound.
func Validate(replicaURL string, policyJSON, versioningJSON, objectLockJSON []byte) error {
	location, err := ParseLocation(replicaURL)
	if err != nil {
		return err
	}

	var versioning BucketVersioning
	if err := json.Unmarshal(versioningJSON, &versioning); err != nil {
		return fmt.Errorf("replica lifecycle: parse bucket versioning: %w", err)
	}
	if versioning.Status != "" {
		return fmt.Errorf("replica lifecycle: bucket versioning is %q; use a never-versioned bucket so historical versions cannot exceed the deletion bound", versioning.Status)
	}

	var objectLock ObjectLockResponse
	if err := json.Unmarshal(objectLockJSON, &objectLock); err != nil {
		return fmt.Errorf("replica lifecycle: parse object-lock configuration: %w", err)
	}
	if objectLock.Configuration.ObjectLockEnabled != "" {
		return fmt.Errorf("replica lifecycle: Object Lock is %q and can retain replicas beyond the deletion bound", objectLock.Configuration.ObjectLockEnabled)
	}

	var policy LifecycleConfiguration
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		return fmt.Errorf("replica lifecycle: parse lifecycle policy: %w", err)
	}

	for _, rule := range policy.Rules {
		prefix := rule.Filter.Prefix
		if prefix == "" {
			prefix = rule.Prefix
		}
		if rule.ID != PolicyID || rule.Status != "Enabled" || prefix != location.Prefix {
			continue
		}
		if rule.Expiration.Days < 1 || rule.Expiration.Days > ExpirationDays {
			return fmt.Errorf("replica lifecycle: %s current-object expiration is %d days, want 1-%d", PolicyID, rule.Expiration.Days, ExpirationDays)
		}
		if rule.NoncurrentVersionExpiration.NoncurrentDays < 1 || rule.NoncurrentVersionExpiration.NoncurrentDays > ExpirationDays {
			return fmt.Errorf("replica lifecycle: %s noncurrent-version expiration is %d days, want 1-%d", PolicyID, rule.NoncurrentVersionExpiration.NoncurrentDays, ExpirationDays)
		}
		if rule.NoncurrentVersionExpiration.NewerNoncurrentVersions != 0 {
			return fmt.Errorf("replica lifecycle: %s retains %d newer noncurrent versions", PolicyID, rule.NoncurrentVersionExpiration.NewerNoncurrentVersions)
		}
		if rule.AbortIncompleteMultipartUpload.DaysAfterInitiation < 1 || rule.AbortIncompleteMultipartUpload.DaysAfterInitiation > ExpirationDays {
			return fmt.Errorf("replica lifecycle: %s incomplete-upload expiration is %d days, want 1-%d", PolicyID, rule.AbortIncompleteMultipartUpload.DaysAfterInitiation, ExpirationDays)
		}

		return nil
	}

	return fmt.Errorf("replica lifecycle: enabled rule %q for prefix %q is absent", PolicyID, location.Prefix)
}

// ValidateAttestation decodes and validates one fresh, atomic provider bundle.
// It fails closed on missing location evidence, clock skew, or any bucket and
// prefix mismatch before applying the lifecycle policy checks.
func ValidateAttestation(replicaURL string, body []byte, now time.Time) error {
	var evidence Attestation
	if err := json.Unmarshal(body, &evidence); err != nil {
		return fmt.Errorf("replica lifecycle: parse attestation: %w", err)
	}
	if evidence.Version != AttestationVersion {
		return fmt.Errorf("replica lifecycle: attestation version %q is not %q", evidence.Version, AttestationVersion)
	}
	if evidence.FetchedAt.IsZero() || evidence.FetchedAt.After(now.UTC().Add(time.Minute)) || now.UTC().Sub(evidence.FetchedAt) > AttestationMaxAge {
		return fmt.Errorf("replica lifecycle: attestation fetched at %s is not fresh", evidence.FetchedAt.UTC().Format(time.RFC3339))
	}

	location, err := ParseLocation(replicaURL)
	if err != nil {
		return err
	}
	if evidence.ReplicaURL != replicaURL || evidence.Bucket != location.Bucket || evidence.Prefix != location.Prefix {
		return fmt.Errorf("replica lifecycle: attestation is bound to %s bucket=%q prefix=%q, not %s", evidence.ReplicaURL, evidence.Bucket, evidence.Prefix, replicaURL)
	}

	var locationResponse map[string]json.RawMessage
	if err := json.Unmarshal(evidence.BucketLocation, &locationResponse); err != nil {
		return fmt.Errorf("replica lifecycle: parse bucket location: %w", err)
	}
	if _, ok := locationResponse["LocationConstraint"]; !ok {
		return fmt.Errorf("replica lifecycle: attestation has no GetBucketLocation evidence")
	}

	return Validate(replicaURL, evidence.Lifecycle, evidence.Versioning, evidence.ObjectLock)
}
