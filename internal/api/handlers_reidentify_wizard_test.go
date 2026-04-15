package api

import (
	"testing"
)

// TestApplyDecision_Idempotent verifies the wizard decision helper. The
// Back button lets a user revisit a step and change their mind, so the
// helper must normalize the session counters against the previous
// step.Decision before recording the new one; same-decision resubmits are
// a no-op.
func TestApplyDecision_Idempotent(t *testing.T) {
	t.Run("no_prior_decision", func(t *testing.T) {
		sess := &reIdentifyWizardSession{}
		step := &reIdentifyWizardStep{}
		applyDecision(sess, step, "accepted")
		if step.Decision != "accepted" {
			t.Errorf("Decision = %q, want accepted", step.Decision)
		}
		if sess.Accepted != 1 || sess.Skipped != 0 || sess.Declined != 0 {
			t.Errorf("counters = (a=%d s=%d d=%d), want (1, 0, 0)",
				sess.Accepted, sess.Skipped, sess.Declined)
		}
	})

	t.Run("resubmit_same_is_noop", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Accepted: 1}
		step := &reIdentifyWizardStep{Decision: "accepted"}
		applyDecision(sess, step, "accepted")
		if sess.Accepted != 1 {
			t.Errorf("Accepted = %d, want 1 (resubmit must no-op)", sess.Accepted)
		}
	})

	t.Run("change_accepted_to_skipped", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Accepted: 1}
		step := &reIdentifyWizardStep{Decision: "accepted"}
		applyDecision(sess, step, "skipped")
		if sess.Accepted != 0 {
			t.Errorf("Accepted = %d, want 0 after change", sess.Accepted)
		}
		if sess.Skipped != 1 {
			t.Errorf("Skipped = %d, want 1 after change", sess.Skipped)
		}
	})

	t.Run("change_skipped_to_declined", func(t *testing.T) {
		sess := &reIdentifyWizardSession{Skipped: 1}
		step := &reIdentifyWizardStep{Decision: "skipped"}
		applyDecision(sess, step, "declined")
		if sess.Skipped != 0 || sess.Declined != 1 {
			t.Errorf("counters = (s=%d d=%d), want (0, 1)", sess.Skipped, sess.Declined)
		}
	})

	t.Run("underflow_guard", func(t *testing.T) {
		// If counters are somehow already zero (e.g. a bug elsewhere), the
		// helper must not go negative. Counters are ints so negatives are
		// representable and would corrupt the summary toast.
		sess := &reIdentifyWizardSession{Accepted: 0}
		step := &reIdentifyWizardStep{Decision: "accepted"}
		applyDecision(sess, step, "skipped")
		if sess.Accepted < 0 {
			t.Errorf("Accepted = %d, must not go negative", sess.Accepted)
		}
	})
}
