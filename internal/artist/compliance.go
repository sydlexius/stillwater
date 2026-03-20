package artist

// ComplianceStatus represents an artist's overall rule compliance level,
// derived from the highest-severity active violation for that artist.
type ComplianceStatus string

const (
	// ComplianceCompliant means no active violations exist for the artist.
	ComplianceCompliant ComplianceStatus = "compliant"

	// ComplianceWarning means the worst active violation is a warning or info.
	ComplianceWarning ComplianceStatus = "warning"

	// ComplianceError means at least one active violation has error severity.
	ComplianceError ComplianceStatus = "error"
)
