package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sydlexius/stillwater/internal/artist"
)

// The merge response carries extrafanart_report only when the merge reports
// one (#3180), under the documented snake_case names.
func TestToMergeResultPayload_ExtraFanartReport(t *testing.T) {
	t.Parallel()
	with, err := json.Marshal(toMergeResultPayload(&artist.MergeResult{
		SurvivorID: "s", ExtraFanart: &artist.ExtraFanartReport{ArtistName: "The Cure", FileCount: 5},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), `"extrafanart_report":{"artist_name":"The Cure","file_count":5}`) {
		t.Errorf("payload = %s, want the extrafanart_report object", with)
	}
	without, _ := json.Marshal(toMergeResultPayload(&artist.MergeResult{SurvivorID: "s"}))
	if strings.Contains(string(without), "extrafanart_report") {
		t.Errorf("payload = %s, want no extrafanart_report when none is reported", without)
	}
}
