package install

import (
	"encoding/json"
	"testing"
)

func TestAccountMigrationProofBindsOldReleaseAndCandidate(t *testing.T) {
	proof := accountMigrationProof{SchemaVersion: 1, PreviousSource: "old", PreviousRelease: "old-release",
		CandidateSource: "new", CandidateDigest: "product", MaterialsDigest: "materials"}
	data, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	report := Report{PreviousSource: proof.PreviousSource, PreviousReleaseID: proof.PreviousRelease,
		SourceRevision: proof.CandidateSource, BundleSHA256: proof.CandidateDigest, MaterialsSHA256: proof.MaterialsDigest}
	if err := verifyAccountMigrationProof(data, report); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		change func(*Report)
	}{
		{"installed release replaced", func(r *Report) { r.PreviousReleaseID = "another" }},
		{"installed revision replaced", func(r *Report) { r.PreviousSource = "another" }},
		{"candidate rebuilt", func(r *Report) { r.BundleSHA256 = "another" }},
		{"materials replaced", func(r *Report) { r.MaterialsSHA256 = "another" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			changed := report
			scenario.change(&changed)
			if err := verifyAccountMigrationProof(data, changed); err == nil {
				t.Fatal("mismatched acceptance record was accepted")
			}
		})
	}
}
