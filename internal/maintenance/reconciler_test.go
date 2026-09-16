package maintenance

import "testing"

func TestDecideReconcile(t *testing.T) {
	const maxAttempts = 5

	cases := []struct {
		name          string
		priorAttempts int
		want          reconcileAction
	}{
		{"no executions (lost enqueue)", 0, reconcileRepublish},
		{"below cap", 3, reconcileRepublish},
		{"one below cap", maxAttempts - 1, reconcileRepublish},
		{"at cap (crash-pill)", maxAttempts, reconcileDeadLetter},
		{"above cap", maxAttempts + 2, reconcileDeadLetter},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideReconcile(tc.priorAttempts, maxAttempts); got != tc.want {
				t.Fatalf("decideReconcile(%d, %d) = %v, want %v", tc.priorAttempts, maxAttempts, got, tc.want)
			}
		})
	}
}
