package state_test

import (
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func TestRunIdentityKeepsOpaqueBackendIdentitySeparate(t *testing.T) {
	for _, test := range []struct {
		run  *models.Run
		want string
	}{
		{nil, ""},
		{&models.Run{}, ""},
		{&models.Run{StorageIdentity: "mongo-objectid:original"}, "mongo-objectid:original"},
		{&models.Run{IncarnationID: "sql-original", StorageIdentity: "ignored"}, "sql-original"},
	} {
		if got := state.RunIdentity(test.run); got != test.want {
			t.Fatalf("identity=%q want=%q", got, test.want)
		}
	}
}
