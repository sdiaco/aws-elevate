package version

import (
	"encoding/json"
	"os"
	"testing"
)

type compatCases struct {
	Outcomes []string `json:"outcomes"`
	Cases    []struct {
		Name      string `json:"name"`
		Client    string `json:"client"`
		Infra     string `json:"infra"`
		MinClient string `json:"minClient"`
		MinInfra  string `json:"minInfra"`
		Expected  string `json:"expected"`
	} `json:"cases"`
}

func loadCases(t *testing.T) compatCases {
	t.Helper()
	raw, err := os.ReadFile("../../../contract/compat.json")
	if err != nil {
		t.Fatal(err)
	}
	var c compatCases
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCompareFollowsTheContract(t *testing.T) {
	for _, c := range loadCases(t).Cases {
		got, err := Compare(c.Client, c.Infra, c.MinClient, c.MinInfra)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if string(got) != c.Expected {
			t.Errorf("%s: got %s, want %s", c.Name, got, c.Expected)
		}
	}
}

func TestOutcomesMatchTheContract(t *testing.T) {
	want := loadCases(t).Outcomes
	got := []Compat{OK, ClientTooOld, InfraTooOld}
	if len(got) != len(want) {
		t.Fatalf("got %d outcomes, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("outcome %d: got %s, want %s", i, got[i], want[i])
		}
	}
}

func TestBothTooOldReportsTheClient(t *testing.T) {
	got, err := Compare("0.1.0", "0.1.0", "0.2.0", "0.2.0")
	if err != nil || got != ClientTooOld {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestInvalidVersionIsAnError(t *testing.T) {
	for _, v := range []string{"", "1.2", "1.2.x", "v1.2.3", "1.2.3.4", "1.-2.3"} {
		if _, err := Compare(v, "0.0.1", "0.0.1", "0.0.1"); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestOwnVersionsAreValid(t *testing.T) {
	if _, err := Compare(Client, MinInfra, Client, MinInfra); err != nil {
		t.Fatal(err)
	}
}
