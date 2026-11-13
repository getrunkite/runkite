package actionexport

import "testing"

func TestRegisteredDestinationIdentityAndTLS(t *testing.T) {
	for _, endpoint := range []string{"http://remote.example.test/events", "https://user:secret@example.test/events", "https://example.test/events?token=synthetic", "file:///tmp/events"} {
		if _, err := (Config{Name: "security", URL: endpoint, AllowInsecureLoopback: true}).SinkID(); err == nil {
			t.Fatal("unsafe evidence destination accepted")
		}
	}
	a := Config{Name: "security", URL: "https://example.test/events"}
	first, err := a.SinkID()
	if err != nil {
		t.Fatal(err)
	}
	a.SecretRef = "env:ROTATED_KEY"
	if second, _ := a.SinkID(); first != second {
		t.Fatal("key rotation stranded delivery queue")
	}
	a.URL += "/new"
	if second, _ := a.SinkID(); first == second {
		t.Fatal("endpoint replacement reused queue")
	}
}
