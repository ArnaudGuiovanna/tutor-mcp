package assessment

import (
	"encoding/json"
	"testing"

	"tutor-mcp/models"
)

func TestCurriculumFindingsRequireExplicitCoverage(t *testing.T) {
	snapshot := &models.CurriculumSnapshot{Concepts: []models.CurriculumConcept{
		{ID: "id-a", Key: "a", Status: models.CurriculumConceptActive}, {ID: "id-b", Key: "b", Status: models.CurriculumConceptActive}}, Graph: models.KnowledgeSpace{Prerequisites: map[string][]string{"b": {"a"}}}}
	findings := []models.CurriculumFinding{{ConceptID: "id-b", Aspect: "prerequisite", RelatedConceptID: "id-a", Judgment: "adequate", Rationale: "The target requires this prerequisite."}}
	for _, c := range snapshot.Concepts {
		for _, aspect := range []string{"definition", "outcomes", "criteria"} {
			findings = append(findings, models.CurriculumFinding{ConceptID: c.ID, Aspect: aspect, Judgment: "adequate", Rationale: "Reviewed this section against the intended domain."})
		}
	}
	raw, _ := json.Marshal(findings)
	_, disposition, reviewed, total, err := ValidateCurriculumFindings(snapshot, string(raw))
	if err != nil || disposition != "complete_opinion" || reviewed != 7 || total != 7 {
		t.Fatalf("coverage %s %d/%d %v", disposition, reviewed, total, err)
	}
	raw, _ = json.Marshal(findings[:1])
	_, disposition, reviewed, total, err = ValidateCurriculumFindings(snapshot, string(raw))
	if err != nil || disposition != "partial_opinion" || reviewed != 1 || total != 7 {
		t.Fatal("partial coverage promoted to complete")
	}
	findings[0].Judgment = "needs_revision"
	raw, _ = json.Marshal(findings)
	_, disposition, _, _, err = ValidateCurriculumFindings(snapshot, string(raw))
	if err != nil || disposition != "changes_requested" {
		t.Fatal("requested changes ignored")
	}
	for _, bad := range []string{
		`[{"concept_id":"foreign","aspect":"definition","judgment":"adequate","rationale":"Reviewed"}]`,
		`[{"concept_id":"id-a","aspect":"definition","judgment":"approved","rationale":"Reviewed"}]`,
		`[{"concept_id":"id-a","aspect":"prerequisite","related_concept_id":"id-b","judgment":"adequate","rationale":"Not an existing edge"}]`,
		`[{"concept_id":"id-a","aspect":"definition","judgment":"adequate","rationale":"","certified":true}]`,
	} {
		if _, _, _, _, err := ValidateCurriculumFindings(snapshot, bad); err == nil {
			t.Fatal("invalid findings accepted")
		}
	}
}
