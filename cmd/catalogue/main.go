// Command catalogue loads medicines into the shared medication catalogue
// from a CSV file. It is a platform operator's tool: the catalogue is
// reference data every practice reads, so practices cannot change it.
//
//	go run ./cmd/catalogue -import medicines.csv
//
// The file needs a header row naming its columns, in any order:
//
//	generic_name,brand_name,strength,dosage_form,route,schedule,nappi_code,pack_size,active
//
// Columns may be separated by commas or semicolons: Excel uses the Windows
// list separator, which is a semicolon wherever the decimal separator is a
// comma.
//
// generic_name, strength and dosage_form are required. route defaults to
// oral and active to true. Importing the same product again (same generic
// name, brand, strength and form) updates it rather than adding a copy.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/yourorg/medical-expert-system/internal/config"
	"github.com/yourorg/medical-expert-system/internal/db"
	"github.com/yourorg/medical-expert-system/internal/service"
)

func main() {
	path := flag.String("import", "", "CSV file to import (required)")
	flag.Parse()
	if *path == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := config.LoadDotEnv(config.EnvFile()); err != nil {
		fail(err)
	}
	f, err := os.Open(*path)
	if err != nil {
		fail(err)
	}
	defer f.Close()

	ctx := context.Background()
	conn, err := db.Open(ctx, os.Getenv("MES_DSN"), db.Options{MaxOpen: 2})
	if err != nil {
		fail(err)
	}
	defer conn.Close()
	if pending, err := db.Pending(ctx, conn); err != nil {
		fail(err)
	} else if len(pending) > 0 {
		fail(fmt.Errorf("the schema is not up to date (%s pending); run `go run ./cmd/migrate` first", strings.Join(pending, ", ")))
	}
	reg, err := service.NewRegistry(service.Deps{DB: conn, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
	if err != nil {
		fail(err)
	}

	br := bufio.NewReader(f)
	r := csv.NewReader(br)
	r.Comma = separator(br)
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		fail(fmt.Errorf("reading the header row: %w", err))
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))] = i
	}
	for _, required := range []string{"generic_name", "strength", "dosage_form"} {
		if _, ok := col[required]; !ok {
			fail(fmt.Errorf("the header row has no %s column", required))
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	added, updated, rejected := 0, 0, 0
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d: %v\n", line, err)
			rejected++
			continue
		}
		active := !strings.EqualFold(get(rec, "active"), "false") && get(rec, "active") != "0"
		inserted, err := reg.ImportMedication(ctx, service.CatalogueRow{
			GenericName: get(rec, "generic_name"), BrandName: get(rec, "brand_name"), Strength: get(rec, "strength"),
			DosageForm: get(rec, "dosage_form"), Route: get(rec, "route"), Schedule: get(rec, "schedule"),
			NAPPICode: get(rec, "nappi_code"), PackSize: get(rec, "pack_size"), Active: active,
		})
		if err != nil {
			var se *service.Error
			if errors.As(err, &se) && len(se.Fields) > 0 {
				for _, f := range se.Fields {
					fmt.Fprintf(os.Stderr, "line %d: %s %s\n", line, f.Field, f.Message)
				}
			} else {
				fmt.Fprintf(os.Stderr, "line %d: %v\n", line, err)
			}
			rejected++
			continue
		}
		if inserted {
			added++
		} else {
			updated++
		}
	}
	fmt.Printf("catalogue import: %d added, %d updated, %d rejected\n", added, updated, rejected)
	if rejected > 0 {
		os.Exit(1)
	}
}

// separator guesses the column separator from the header row: whichever of
// comma and semicolon it contains more of.
func separator(br *bufio.Reader) rune {
	head, _ := br.Peek(br.Size()) // fewer bytes and io.EOF for a short file
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	if bytes.Count(head, []byte(";")) > bytes.Count(head, []byte(",")) {
		return ';'
	}
	return ','
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "catalogue:", err)
	os.Exit(1)
}
