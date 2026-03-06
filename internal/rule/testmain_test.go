package rule

import (
	"os"
	"testing"

	"github.com/sydlexius/stillwater/internal/database"
)

var templateDBPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "rule-test-template-*")
	if err != nil {
		panic("creating temp dir: " + err.Error())
	}

	templateDBPath = dir + "/template.db"
	db, err := database.Open(templateDBPath)
	if err != nil {
		panic("opening template db: " + err.Error())
	}
	if err := database.Migrate(db); err != nil {
		panic("migrating template db: " + err.Error())
	}
	_ = db.Close()

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
