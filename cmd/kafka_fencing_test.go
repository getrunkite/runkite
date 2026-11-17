package main

import "testing"

func TestKafkaNonAtomicFencingRequiresExplicitTrue(t *testing.T) {
	for _, value := range []string{"", "false", "true", "1", "TRUE", "True", " true", "true ", "yes", "0"} {
		t.Run(value, func(t *testing.T) {
			allowed, err := kafkaNonAtomicFencingEnabled(value)
			valid := value == "" || value == "false" || value == "true"
			if (err == nil) != valid || allowed != (value == "true") {
				t.Fatalf("value=%q allowed=%t err=%v", value, allowed, err)
			}
		})
	}
}
