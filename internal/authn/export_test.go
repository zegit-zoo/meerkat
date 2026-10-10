package authn

// DropPolicyForTest removes the verifier's policy, reproducing the
// "providers configured, policy missing" state the gate must fail closed
// on.
func DropPolicyForTest(v *Verifier) { v.policy = nil }
