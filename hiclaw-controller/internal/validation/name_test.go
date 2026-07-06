package validation

import "testing"

func TestValidateResourceName(t *testing.T) {
	valid := []string{"a", "worker1", "alpha-worker", "a1-b2-c3", "9worker"}
	for _, name := range valid {
		if err := ValidateResourceName(name); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", name, err)
		}
	}

	invalid := []string{
		"",
		"-worker",
		"Worker",
		"worker_name",
		"worker name",
		"worker.name",
		"WORKER",
		"-",
		"worker!",
	}
	for _, name := range invalid {
		if err := ValidateResourceName(name); err == nil {
			t.Errorf("expected %q to be invalid, got nil error", name)
		}
	}
}
