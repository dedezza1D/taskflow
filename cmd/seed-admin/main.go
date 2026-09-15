// Command seed-admin bootstraps the first organisation and its admin.
//
// There is no public sign-up: this tool is how the first account comes into
// existence, and every account after it is created by an admin from inside the
// application. Running it twice against the same email is refused rather than
// silently resetting a password.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/config"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
)

func main() {
	var (
		orgName  = flag.String("org", "", "organisation name (required)")
		email    = flag.String("email", "", "admin email (required)")
		password = flag.String("password", "", "admin password (optional; generated when omitted)")
		orgID    = flag.String("org-id", auth.LocalOrgID.String(),
			"organisation id; defaults to the legacy org so pre-auth documents stay reachable")
	)
	flag.Parse()

	if *orgName == "" || *email == "" {
		flag.Usage()
		os.Exit(2)
	}

	cfg := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fail("connect: %v", err)
	}
	defer st.Close()

	oid, err := uuid.Parse(*orgID)
	if err != nil {
		fail("invalid --org-id: %v", err)
	}

	// The legacy organisation is already seeded by migration 003; adopting it
	// rather than making a new one is what keeps documents created before auth
	// visible to this admin instead of orphaning them.
	if _, err := st.GetOrganization(ctx, oid); errors.Is(err, store.ErrNotFound) {
		if _, err := st.CreateOrganization(ctx, oid, *orgName); err != nil {
			fail("create organisation: %v", err)
		}
		fmt.Printf("organisation created: %s (%s)\n", *orgName, oid)
	} else if err != nil {
		fail("load organisation: %v", err)
	} else {
		fmt.Printf("using existing organisation %s\n", oid)
	}

	plain := *password
	generated := false
	if plain == "" {
		if plain, err = generatePassword(); err != nil {
			fail("generate password: %v", err)
		}
		generated = true
	}

	hash, err := auth.HashPassword(plain)
	if err != nil {
		fail("%v", err)
	}

	user, err := st.CreateUser(ctx, store.CreateUserParams{
		OrgID:        oid,
		Email:        auth.NormalizeEmail(*email),
		PasswordHash: hash,
		Role:         store.RoleAdmin,
	})
	if errors.Is(err, store.ErrEmailTaken) {
		fail("%s already exists; this tool will not reset an existing account", *email)
	}
	if err != nil {
		fail("create user: %v", err)
	}

	fmt.Printf("admin created: %s (%s)\n", user.Email, user.ID)
	if generated {
		// Printed once, to stdout, never stored anywhere. Change it after the
		// first sign-in — the application has an endpoint for that.
		fmt.Printf("\n  password: %s\n\n", plain)
		fmt.Println("Shown once. Sign in and change it.")
	}
}

func generatePassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "seed-admin: "+format+"\n", args...)
	os.Exit(1)
}
