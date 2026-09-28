// Command bootstrap creates a practice and its first administrator.
//
//	go run ./cmd/bootstrap -slug greenside -name "Greenside Clinic" \
//	    -admin-email admin@greenside.co.za -admin-name "Jane Admin"
//
// Leave -admin-password empty and a strong one is generated and printed
// once. Passing a password on the command line leaves it in shell history.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/yourorg/medical-expert-system/internal/config"
	"github.com/yourorg/medical-expert-system/internal/db"
	"github.com/yourorg/medical-expert-system/internal/service"
)

func main() {
	var in service.ProvisionInput
	flag.StringVar(&in.Slug, "slug", "", "practice code users type at sign-in, e.g. greenside (required)")
	flag.StringVar(&in.Name, "name", "", "practice name as printed on letters (required)")
	flag.StringVar(&in.AdminEmail, "admin-email", "", "first administrator's email (required)")
	flag.StringVar(&in.AdminName, "admin-name", "", "first administrator's full name (required)")
	flag.StringVar(&in.AdminPassword, "admin-password", "", "initial password; leave empty to generate one")
	flag.StringVar(&in.Timezone, "timezone", "Africa/Johannesburg", "practice time zone")
	flag.StringVar(&in.PracticeNumber, "practice-number", "", "BHF practice number")
	flag.StringVar(&in.Address, "address", "", "practice address")
	flag.StringVar(&in.Phone, "phone", "", "practice telephone")
	flag.StringVar(&in.Email, "email", "", "practice email")
	flag.Parse()

	if err := config.LoadDotEnv(config.EnvFile()); err != nil {
		fail(err)
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, os.Getenv("MES_DSN"), db.Options{MaxOpen: 2})
	if err != nil {
		fail(err)
	}
	defer conn.Close()

	pending, err := db.Pending(ctx, conn)
	if err != nil {
		fail(err)
	}
	if len(pending) > 0 {
		fail(fmt.Errorf("the schema is not up to date (%s pending); run `go run ./cmd/migrate` first",
			strings.Join(pending, ", ")))
	}

	reg, err := service.NewRegistry(service.Deps{DB: conn, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		fail(err)
	}
	res, err := reg.Provision(ctx, in)
	if err != nil {
		var se *service.Error
		if errors.As(err, &se) && len(se.Fields) > 0 {
			for _, f := range se.Fields {
				fmt.Fprintf(os.Stderr, "  -%s %s\n", strings.ReplaceAll(f.Field, "_", "-"), f.Message)
			}
			os.Exit(1)
		}
		fail(err)
	}

	fmt.Printf("Practice created\n")
	fmt.Printf("  practice code : %s\n", res.Tenant.Slug)
	fmt.Printf("  name          : %s\n", res.Tenant.Name)
	fmt.Printf("  administrator : %s\n", res.Admin.Email)
	if res.GeneratedPassword != "" {
		fmt.Printf("  password      : %s\n", res.GeneratedPassword)
		fmt.Printf("\nThis password is shown once. Sign in, change it, and set up two-step verification.\n")
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "bootstrap:", err)
	os.Exit(1)
}
