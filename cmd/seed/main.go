// Command seed builds a demo practice of made-up patients with months of
// history, by running the real system on a simulated clock.
//
//	go run ./cmd/seed -slug demo -name "Greenside Clinic"
//
// Everything goes through the services the API uses, business rules and
// audit included, while a clock moves forward through the history: patients
// registered, medicines started, notes signed, scripts issued, results
// captured. From the day the practice switched WhatsApp on, the real dose
// scheduler and reminder dispatcher run against an in-process stand-in for
// WhatsApp, and simulated patients answer, or do not, the way real ones do.
//
// The practice is marked as a demo. Its patients' names, ID numbers and
// phone numbers are invented, so a demo practice never sends a message,
// whatever the server is configured with.
package main

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/medical-expert-system/internal/config"
	"github.com/yourorg/medical-expert-system/internal/db"
	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/notify"
	"github.com/yourorg/medical-expert-system/internal/secure"
	"github.com/yourorg/medical-expert-system/internal/service"
	"github.com/yourorg/medical-expert-system/internal/tenant"
)

type options struct {
	slug, name, password string
	days                 int
	seed                 int64
}

func main() {
	var o options
	flag.StringVar(&o.slug, "slug", "demo", "practice code users type at sign-in")
	flag.StringVar(&o.name, "name", "Greenside Clinic", "practice name")
	flag.IntVar(&o.days, "days", 60, "days of reminder history to simulate, 14 to 120")
	flag.StringVar(&o.password, "password", "", "one password for every demo login; empty generates one each")
	flag.Int64Var(&o.seed, "seed", 1, "random seed: the same seed builds the same history")
	flag.Parse()
	if o.days < 14 || o.days > 120 {
		fail(errors.New("-days must be between 14 and 120"))
	}
	if err := config.LoadDotEnv(config.EnvFile()); err != nil {
		fail(err)
	}
	ctx := context.Background()
	conn, err := db.Open(ctx, os.Getenv("MES_DSN"), db.Options{MaxOpen: 4})
	if err != nil {
		fail(err)
	}
	defer conn.Close()
	if pending, err := db.Pending(ctx, conn); err != nil {
		fail(err)
	} else if len(pending) > 0 {
		fail(fmt.Errorf("the schema is not up to date (%s pending); run `go run ./cmd/migrate` first", strings.Join(pending, ", ")))
	}
	s := &seeder{ctx: ctx, opt: o, rnd: rand.New(rand.NewSource(o.seed)), end: time.Now().UTC().Truncate(time.Second)}
	s.clock = &simClock{t: s.end}
	if err := s.run(conn); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "seed:", err)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// The clock, and a stand-in for WhatsApp
// ---------------------------------------------------------------------------

type simClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *simClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *simClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.t) {
		c.t = t
	}
}

type outgoing struct {
	to, providerID string
	template       string
	payloads       []string
	at             time.Time
}

// standIn accepts messages the way the Cloud API would and remembers them,
// so the simulation can decide how each patient responds.
type standIn struct {
	clock    *simClock
	rnd      *rand.Rand
	mu       sync.Mutex
	outbox   []outgoing
	unusable map[string]bool // numbers not on WhatsApp
	seq      int
}

func (s *standIn) Language() string { return "en" }

func (s *standIn) Send(_ context.Context, m notify.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unusable[m.To] {
		return "", &notify.SendError{Permanent: true, Status: 400, Code: "131026",
			Detail: "(#131026) Message undeliverable: the number is not on WhatsApp"}
	}
	if s.rnd.Float64() < 0.01 {
		return "", &notify.SendError{Status: 503, Code: "131016", Detail: "Service temporarily unavailable"}
	}
	s.seq++
	id := fmt.Sprintf("wamid.demo%s%06d", strings.ReplaceAll(model.MustNewID().String()[:8], "-", ""), s.seq)
	s.outbox = append(s.outbox, outgoing{to: m.To, providerID: id, template: m.Template.Name,
		payloads: m.ButtonPayloads, at: s.clock.Now()})
	return id, nil
}

func (s *standIn) drain() []outgoing {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.outbox
	s.outbox = nil
	return out
}

// ---------------------------------------------------------------------------
// Events, run in time order
// ---------------------------------------------------------------------------

type event struct {
	at   time.Time
	seq  int
	name string
	run  func() error
}

type queue []*event

func (q queue) Len() int { return len(q) }
func (q queue) Less(i, j int) bool {
	if q[i].at.Equal(q[j].at) {
		return q[i].seq < q[j].seq
	}
	return q[i].at.Before(q[j].at)
}
func (q queue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *queue) Push(x any)   { *q = append(*q, x.(*event)) }
func (q *queue) Pop() any {
	old := *q
	e := old[len(old)-1]
	*q = old[:len(old)-1]
	return e
}

// ---------------------------------------------------------------------------
// The seeder
// ---------------------------------------------------------------------------

type seeder struct {
	ctx    context.Context
	opt    options
	rnd    *rand.Rand
	end    time.Time
	clock  *simClock
	reg    *service.Registry
	scope  *tenant.Scope
	tenant model.ID
	sender *standIn
	sast   *time.Location

	admin, doctor, nurse service.Actor
	logins               []login
	patients             []*demoPatient
	byMobile             map[string]*demoPatient
	events               queue
	seq                  int
	medicines            map[string]model.ID

	sent, replies, appAnswers int
}

type login struct{ who, email, password string }

func (s *seeder) at(t time.Time, name string, fn func() error) {
	s.seq++
	heap.Push(&s.events, &event{at: t, seq: s.seq, name: name, run: fn})
}

// day returns 00:00 SAST, n days before the end.
func (s *seeder) day(n int) time.Time {
	y, m, d := s.end.In(s.sast).AddDate(0, 0, -n).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, s.sast)
}

func (s *seeder) as(a service.Actor) *service.Services {
	return s.reg.For(s.scope, a, service.StateActive)
}

func (s *seeder) password() string {
	if s.opt.password != "" {
		return s.opt.password
	}
	p, err := secure.RandomPassword()
	if err != nil {
		fail(err)
	}
	return p
}

func (s *seeder) run(conn *sql.DB) error {
	sast, err := time.LoadLocation("Africa/Johannesburg")
	if err != nil {
		sast = time.FixedZone("SAST", 2*3600)
	}
	s.sast = sast
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	historyStart := s.day(330).Add(9 * time.Hour)
	s.clock = &simClock{t: historyStart}

	// Provision with a registry that cannot send, then simulate with one
	// that sends only to the stand-in and only for this practice.
	boot, err := service.NewRegistry(service.Deps{DB: conn, Logger: quiet, Now: s.clock.Now})
	if err != nil {
		return err
	}
	adminPassword := s.password()
	res, err := boot.Provision(s.ctx, service.ProvisionInput{
		Slug: s.opt.slug, Name: s.opt.name, Timezone: "Africa/Johannesburg", PracticeNumber: "0123456",
		Address: "12 Main Road, Greenside, Johannesburg", Phone: "011 555 0123", Email: "reception@" + s.opt.slug + ".example",
		AdminEmail: "admin@" + s.opt.slug + ".example", AdminName: "Jane Admin", AdminPassword: adminPassword,
	})
	if err != nil {
		var se *service.Error
		if errors.As(err, &se) && se.Code == "slug_taken" {
			return fmt.Errorf("a practice with the code %q already exists; choose another with -slug", s.opt.slug)
		}
		return err
	}
	s.tenant = res.Tenant.ID
	s.logins = append(s.logins, login{"Practice administrator", res.Admin.Email, adminPassword})
	s.sender = &standIn{clock: s.clock, rnd: rand.New(rand.NewSource(s.opt.seed + 1)), unusable: map[string]bool{}}
	s.reg, err = service.NewRegistry(service.Deps{DB: conn, Logger: quiet, Now: s.clock.Now,
		Sender: s.sender, PhoneNumberID: "demo", AllowDemoSends: true, OnlyTenant: s.tenant,
		Config: service.Config{FollowUpAfter: time.Hour}})
	if err != nil {
		return err
	}
	if s.scope, err = tenant.NewScope(conn, s.tenant); err != nil {
		return err
	}
	// Demo from the start: nothing this practice holds is real.
	if err := s.reg.ConfigureDemoPractice(s.ctx, s.tenant, false); err != nil {
		return err
	}
	s.admin = service.Actor{UserID: res.Admin.ID, Role: model.RoleClinicAdmin, RequestID: "seed"}
	if err := s.staff(); err != nil {
		return err
	}
	if err := s.loadMedicines(); err != nil {
		return err
	}
	s.byMobile = map[string]*demoPatient{}
	s.patients = demoPatients()
	fmt.Fprintf(os.Stderr, "Building %s: %d patients, %d days of reminders...\n", s.opt.name, len(s.patients), s.opt.days)
	if err := s.register(historyStart); err != nil {
		return err
	}
	s.planClinicalHistory()
	s.planSimulation()

	for s.events.Len() > 0 {
		e := heap.Pop(&s.events).(*event)
		s.clock.Set(e.at)
		if err := e.run(); err != nil {
			return fmt.Errorf("%s at %s: %w", e.name, e.at.In(s.sast).Format("2006-01-02 15:04"), err)
		}
	}
	s.clock.Set(s.end)
	// Messaging off again: from here on the demo practice is shown, not run.
	if err := s.reg.ConfigureDemoPractice(s.ctx, s.tenant, false); err != nil {
		return err
	}
	s.report()
	return nil
}

func (s *seeder) staff() error {
	users := s.as(s.admin).Users
	for _, st := range []struct {
		who, email, name string
		role             model.Role
		reg              string
		into             *service.Actor
	}{
		{"Doctor", "naledi.khoza", "Dr Naledi Khoza", model.RoleDoctor, "MP0712345", &s.doctor},
		{"Nurse", "ruth.pillay", "Sister Ruth Pillay", model.RoleNurse, "NU1234567", &s.nurse},
	} {
		pw := s.password()
		u, err := users.Create(s.ctx, service.CreateUserInput{Email: st.email + "@" + s.opt.slug + ".example",
			FullName: st.name, Role: st.role, ProfessionalRegNo: st.reg, Password: pw})
		if err != nil {
			return fmt.Errorf("creating %s: %w", st.who, err)
		}
		*st.into = service.Actor{UserID: u.ID, Role: st.role, RequestID: "seed"}
		s.logins = append(s.logins, login{st.who + ", " + st.name, u.Email, pw})
	}
	return nil
}

func (s *seeder) loadMedicines() error {
	s.medicines = map[string]model.ID{}
	for _, q := range []string{"Metformin", "Gliclazide", "Amlodipine", "Enalapril", "Hydrochlorothiazide", "Atorvastatin",
		"Levothyroxine", "Carbamazepine", "Budesonide", "Salbutamol", "Allopurinol", "Fluoxetine"} {
		meds, err := s.as(s.doctor).Medication.SearchCatalogue(s.ctx, q, 25, 0)
		if err != nil {
			return err
		}
		for _, m := range meds {
			s.medicines[m.GenericName+" "+m.Strength] = m.ID
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Registration, and the clinical history before reminders began
// ---------------------------------------------------------------------------

func luhnDigit(digits string) byte {
	sum := 0
	double := true
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return byte('0' + (10-sum%10)%10)
}

func (s *seeder) saID(dob model.Date, sex model.Sex) string {
	seq := s.rnd.Intn(5000)
	if sex == model.SexMale {
		seq += 5000
	}
	base := fmt.Sprintf("%02d%02d%02d%04d08", dob.Year%100, int(dob.Month), dob.Day, seq)
	return base + string(luhnDigit(base))
}

func (s *seeder) mobile() (typed, e164 string) {
	for {
		prefix := []string{"72", "73", "76", "79", "82", "83", "84"}[s.rnd.Intn(7)]
		digits := fmt.Sprintf("%07d", s.rnd.Intn(10_000_000))
		e := "+27" + prefix + digits
		if s.byMobile[e] == nil {
			return "0" + prefix + " " + digits[:3] + " " + digits[3:], e
		}
	}
}

func (s *seeder) register(start time.Time) error {
	for i, p := range s.patients {
		s.clock.Set(start.Add(time.Duration(i) * 7 * time.Hour))
		typed, e164 := s.mobile()
		in := service.PatientInput{FirstName: p.first, LastName: p.last, DateOfBirth: p.dob, Sex: p.sex,
			IDType: model.IDTypeSAID, NationalID: s.saID(p.dob, p.sex), Mobile: typed, PreferredChannel: p.channel,
			PreferredLanguage: "English", SmokingStatus: model.SmokingNever, AlcoholUse: model.AlcoholOccasional}
		if p.waking != nil {
			a, _ := model.ParseTimeOfDay(p.waking[0])
			b, _ := model.ParseTimeOfDay(p.waking[1])
			in.WakingStart, in.WakingEnd = &a, &b
		}
		created, err := s.as(s.admin).Patients.Create(s.ctx, in, true)
		if err != nil {
			return fmt.Errorf("registering %s %s: %w", p.first, p.last, err)
		}
		p.id, p.e164 = created.ID, e164
		s.byMobile[e164] = p
		if p.badNumber {
			s.sender.unusable[e164] = true
		}
		yes, no := true, false
		consents := []service.ConsentInput{{Purpose: model.ConsentDataProcessing, Granted: &yes, Method: model.ConsentWritten}}
		if p.channel == model.ChannelWhatsApp {
			granted := &yes
			if !p.reminders {
				granted = &no
			}
			consents = append(consents, service.ConsentInput{Purpose: model.ConsentReminderComms, Granted: granted, Method: model.ConsentWritten})
		}
		if p.ai {
			consents = append(consents, service.ConsentInput{Purpose: model.ConsentAIAssistedReview, Granted: &yes, Method: model.ConsentWritten})
		}
		for _, c := range consents {
			if _, err := s.as(s.admin).Patients.RecordConsent(s.ctx, p.id, c); err != nil {
				return err
			}
		}
		for _, c := range p.conditions {
			if _, err := s.as(s.doctor).Patients.AddCondition(s.ctx, p.id, service.ConditionInput{
				ICD10Code: c.code, Description: c.name, IsChronic: c.chronic}); err != nil {
				return err
			}
		}
		for _, a := range p.allergies {
			if _, err := s.as(s.nurse).Patients.AddAllergy(s.ctx, p.id, service.AllergyInput{
				Substance: a[0], Reaction: a[1], Severity: model.AllergySeverity(a[2])}); err != nil {
				return err
			}
		}
		if p.portal {
			pw := s.password()
			u, err := s.as(s.admin).Users.Create(s.ctx, service.CreateUserInput{
				Email: strings.ToLower(p.first+"."+p.last) + "@" + s.opt.slug + ".example", FullName: p.first + " " + p.last,
				Role: model.RolePatient, PatientID: &p.id, Password: pw})
			if err != nil {
				return err
			}
			p.actor = service.Actor{UserID: u.ID, Role: model.RolePatient, PatientID: &p.id, RequestID: "seed"}
			s.logins = append(s.logins, login{"Patient, " + p.first + " " + p.last, u.Email, pw})
		}
	}
	return nil
}

// planClinicalHistory queues everything the practice recorded by hand:
// medicines started, reviews, scripts and repeats, results, documents.
func (s *seeder) planClinicalHistory() {
	for _, p := range s.patients {
		p := p
		// Medicines start with the first review, or a year ago.
		first := 300
		if len(p.reviews) > 0 {
			first = p.reviews[0]
		}
		s.at(s.day(first).Add(9*time.Hour+30*time.Minute), "start medicines for "+p.first, func() error {
			for _, m := range p.meds {
				g, err := s.as(s.doctor).Medication.CreateRegimen(s.ctx, p.id, service.RegimenInput{
					MedicationID: s.medicines[m.medicine], Dose: m.dose, Frequency: m.frequency, Instructions: m.instructions})
				if err != nil {
					return fmt.Errorf("%s: %w", m.medicine, err)
				}
				p.regimens = append(p.regimens, g.ID)
			}
			return nil
		})
		for i, ago := range p.reviews {
			last := i == len(p.reviews)-1
			s.at(s.day(ago).Add(10*time.Hour+time.Duration(s.rnd.Intn(300))*time.Minute), "review "+p.first, func() error {
				return s.review(p, last)
			})
		}
		for _, l := range p.labs {
			l := l
			s.at(s.day(l.ago).Add(15*time.Hour), "result for "+p.first, func() error { return s.labResult(p, l) })
		}
		if p.draftNote {
			s.at(s.end.Add(-2*time.Hour), "draft note for "+p.first, func() error {
				_, err := s.as(s.doctor).Encounters.Create(s.ctx, p.id, service.EncounterInput{Type: model.EncounterFollowUp,
					PresentingComplaint: "Came in about tiredness and thirst; to finish after the results."})
				return err
			})
		}
	}
	s.planAcuteVisits()
}

func (s *seeder) review(p *demoPatient, last bool) error {
	sbp, dbp, pulse := 118+s.rnd.Intn(30), 72+s.rnd.Intn(18), 64+s.rnd.Intn(20)
	weight := 58.0 + float64(s.rnd.Intn(45))
	e, err := s.as(s.doctor).Encounters.Create(s.ctx, p.id, service.EncounterInput{
		Type: model.EncounterFollowUp, PresentingComplaint: "Routine chronic care review.",
		History:     "No new complaints. Taking medication as prescribed; no side effects reported.",
		Examination: fmt.Sprintf("BP %d/%d, pulse %d, weight %.0f kg. Heart and chest normal.", sbp, dbp, pulse, weight),
		Assessment:  "Stable on current treatment.", Plan: "Continue current medication. Review in six months.",
		Vitals: &model.Vitals{SystolicBP: &sbp, DiastolicBP: &dbp, PulseRate: &pulse, WeightKg: &weight},
	})
	if err != nil {
		return err
	}
	if _, err := s.as(s.doctor).Encounters.Sign(s.ctx, e.ID); err != nil {
		return err
	}
	if !last || p.script == nil || len(p.regimens) == 0 {
		return nil
	}
	var items []service.PrescriptionItemInput
	for _, id := range p.regimens {
		items = append(items, service.PrescriptionItemInput{RegimenID: id, Quantity: "30-day supply"})
	}
	rx, err := s.as(s.doctor).Prescriptions.Issue(s.ctx, p.id, service.PrescriptionInput{Items: items, RepeatsAuthorised: p.script.repeats})
	if err != nil {
		return fmt.Errorf("script: %w", err)
	}
	for _, ago := range p.script.repeatsAt {
		id := rx.Prescription.ID
		s.at(s.day(ago).Add(11*time.Hour), "repeat for "+p.first, func() error {
			_, err := s.as(s.doctor).Prescriptions.Repeat(s.ctx, id, "")
			return err
		})
	}
	return nil
}

func (s *seeder) labResult(p *demoPatient, l demoLab) error {
	items := make([]service.LabItemInput, 0, len(l.items))
	for _, it := range l.items {
		items = append(items, service.LabItemInput{Analyte: it[0], Value: it[1], Unit: it[2], ReferenceRange: it[3],
			AbnormalFlag: model.AbnormalFlag(it[4])})
	}
	r, err := s.as(s.nurse).Labs.Record(s.ctx, p.id, service.LabResultInput{LabName: "City Pathology",
		LabReference: fmt.Sprintf("CP-%06d", s.rnd.Intn(1_000_000)), Panel: l.panel, Items: items})
	if err != nil {
		return err
	}
	if l.ackNote == "" {
		return nil
	}
	s.at(s.clock.Now().Add(20*time.Hour), "acknowledge a result for "+p.first, func() error {
		_, err := s.as(s.doctor).Labs.Acknowledge(s.ctx, r.ID, l.ackNote)
		return err
	})
	return nil
}

func (s *seeder) planAcuteVisits() {
	find := func(first string) *demoPatient {
		for _, p := range s.patients {
			if p.first == first {
				return p
			}
		}
		panic("no demo patient " + first)
	}
	andile, lerato, thabo, sipho := find("Andile"), find("Lerato"), find("Thabo"), find("Sipho")
	s.at(s.day(12).Add(10*time.Hour), "visit Lerato", func() error {
		return s.acuteVisit(lerato, "Sore throat, runny nose and a mild fever for two days.",
			"Temperature 37.9 °C. Throat inflamed, no exudate. Chest clear.", "Viral upper respiratory tract infection.",
			"Fluids, rest and paracetamol. Return if not better in five days.", 2, "")
	})
	s.at(s.day(5).Add(8*time.Hour+40*time.Minute), "visit Andile", func() error {
		return s.acuteVisit(andile, "Twisted his right ankle playing soccer yesterday.",
			"Swelling over the lateral ankle; can bear weight. No bony tenderness.", "Sprained right ankle.",
			"Rest, ice, strapping and anti-inflammatories. Back to training in two weeks.", 3, "E-4471")
	})
	s.at(s.day(40).Add(11*time.Hour+30*time.Minute), "referral for Thabo", func() error {
		d, err := s.as(s.doctor).Documents.CreateReferral(s.ctx, thabo.id, service.ReferralInput{
			ToName: "Dr S. Pillay", ToSpeciality: "Ophthalmology", ToPractice: "Rosebank Eye Centre", Urgency: "routine",
			Reason:            "Annual diabetic eye screening.",
			ClinicalSummary:   "Type 2 diabetes for eight years, on metformin and gliclazide. Last HbA1c 7.9%.",
			IncludeMedication: true, IncludeAllergies: true})
		if err != nil {
			return err
		}
		_, err = s.as(s.doctor).Documents.Issue(s.ctx, d.ID)
		return err
	})
	s.at(s.day(2).Add(10*time.Hour), "recall for Sipho", func() error {
		_, err := s.as(s.admin).Notifications.SendRecall(s.ctx, sipho.id)
		return err
	})
}

func (s *seeder) acuteVisit(p *demoPatient, complaint, exam, assessment, plan string, daysOff int, employee string) error {
	e, err := s.as(s.doctor).Encounters.Create(s.ctx, p.id, service.EncounterInput{Type: model.EncounterConsultation,
		PresentingComplaint: complaint, Examination: exam, Assessment: assessment, Plan: plan})
	if err != nil {
		return err
	}
	if _, err := s.as(s.doctor).Encounters.Sign(s.ctx, e.ID); err != nil {
		return err
	}
	today := model.DateOf(s.clock.Now().In(s.sast))
	to := model.DateOf(s.clock.Now().In(s.sast).AddDate(0, 0, daysOff-1))
	d, err := s.as(s.doctor).Documents.CreateSickNote(s.ctx, p.id, service.SickNoteInput{Basis: model.BasisExamination,
		ExaminedOn: today, ValidFrom: today, ValidTo: to, CertificateFor: "work", EmployeeNumber: employee, EncounterID: &e.ID})
	if err != nil {
		return err
	}
	_, err = s.as(s.doctor).Documents.Issue(s.ctx, d.ID)
	return err
}

// ---------------------------------------------------------------------------
// The reminder simulation
// ---------------------------------------------------------------------------

func (s *seeder) planSimulation() {
	start := s.day(s.opt.days)
	s.at(start.Add(7*time.Hour), "switch WhatsApp on", func() error {
		return s.reg.ConfigureDemoPractice(s.ctx, s.tenant, true)
	})
	for t := start.Add(7*time.Hour + 5*time.Minute); t.Before(s.end); t = t.Add(15 * time.Minute) {
		local := t.In(s.sast)
		if h := local.Hour(); h >= 5 {
			s.at(t, "dispatch", s.dispatch)
		}
		if local.Minute() == 5 {
			s.at(t.Add(time.Minute), "close overdue doses", func() error {
				_, err := s.reg.CloseOverdueDoses(s.ctx)
				return err
			})
		}
		if local.Hour() == 0 && local.Minute() == 5 {
			s.at(t.Add(2*time.Minute), "schedule doses", func() error {
				_, err := s.reg.GenerateDoses(s.ctx)
				return err
			})
		}
		if h := local.Hour(); (h == 9 || h == 13 || h == 17 || h == 21) && local.Minute() == 35 {
			s.at(t, "answers in the app", s.portalAnswers)
		}
	}
}

func (s *seeder) dispatch() error {
	if _, err := s.reg.DispatchReminders(s.ctx); err != nil {
		return err
	}
	for _, m := range s.sender.drain() {
		s.sent++
		p := s.byMobile[m.to]
		if p == nil {
			continue
		}
		s.react(p, m)
	}
	return nil
}

// react decides how a patient responds to a message: receipts, and perhaps
// a reply, in their own time.
func (s *seeder) react(p *demoPatient, m outgoing) {
	b := profiles[p.profile]
	delivered := m.at.Add(time.Duration(3+s.rnd.Intn(60)) * time.Second)
	s.at(delivered, "delivered", func() error { return s.receipt(m.providerID, "delivered") })
	if m.template == notify.Recall.Name {
		s.at(delivered.Add(time.Duration(10+s.rnd.Intn(600))*time.Minute), "read", func() error { return s.receipt(m.providerID, "read") })
		return
	}
	if m.template == notify.DoseFollowUp.Name || s.rnd.Float64() >= b.answer {
		if s.rnd.Float64() < 0.5 {
			s.at(delivered.Add(time.Duration(5+s.rnd.Intn(300))*time.Minute), "read", func() error { return s.receipt(m.providerID, "read") })
		}
		return
	}
	delay := time.Duration(b.minDelay+s.rnd.Intn(b.maxDelay-b.minDelay+1)) * time.Minute
	replyAt := m.at.Add(delay)
	s.at(replyAt.Add(-30*time.Second), "read", func() error { return s.receipt(m.providerID, "read") })
	answer := notify.DosePayloads(strings.SplitN(m.payloads[0], ":", 2)[0])[1]
	text := "Skip"
	if s.rnd.Float64() < b.take {
		answer, text = m.payloads[0], "Taken"
	}
	in := notify.InboundMessage{PhoneNumberID: "demo", ID: "wamid.in" + strings.ReplaceAll(model.MustNewID().String(), "-", "")[:20],
		From: p.e164, At: replyAt, Type: "button", Text: text, ButtonPayload: answer}
	switch r := s.rnd.Float64(); {
	case r < 0.02:
		in.Type, in.ButtonPayload = "text", ""
		in.Text = []string{"I'm not sure, can I take it later?", "Ran out of pills, will collect tomorrow", "ok"}[s.rnd.Intn(3)]
	case r < b.typed:
		in.Type, in.ButtonPayload = "text", ""
		in.Text = map[string]string{"Taken": []string{"TAKEN", "Taken", "taken ✅", "Done"}[s.rnd.Intn(4)],
			"Skip": []string{"SKIP", "Skip", "skipped"}[s.rnd.Intn(3)]}[text]
	}
	s.at(replyAt, "reply", func() error {
		s.replies++
		_, err := s.reg.HandleWhatsAppEvent(s.ctx, &notify.Event{Messages: []notify.InboundMessage{in}})
		return err
	})
}

func (s *seeder) receipt(providerID, status string) error {
	_, err := s.reg.HandleWhatsAppEvent(s.ctx, &notify.Event{Statuses: []notify.StatusUpdate{
		{PhoneNumberID: "demo", MessageID: providerID, Status: status, At: s.clock.Now()}}})
	return err
}

// chance turns a dose and a question into a fixed number in [0, 1), so a
// patient who looks at the app several times a day decides about each dose
// once.
func (s *seeder) chance(id model.ID, question string) float64 {
	h := fnv.New64a()
	_, _ = h.Write(id.Bytes())
	_, _ = h.Write([]byte(question))
	_, _ = fmt.Fprint(h, s.opt.seed)
	return float64(h.Sum64()%1_000_000) / 1_000_000
}

// portalAnswers: patients with their own login answer in the app a few times
// a day, for doses that have come due.
func (s *seeder) portalAnswers() error {
	now := s.clock.Now()
	for _, p := range s.patients {
		if !p.portal {
			continue
		}
		b := profiles[p.profile]
		svc := s.as(p.actor).Medication
		doses, err := svc.Doses(s.ctx, p.id, now.Add(-16*time.Hour), now)
		if err != nil {
			return err
		}
		for _, d := range doses {
			if d.Status != model.DosePending || d.ScheduledAt.After(now) || s.chance(d.ID, "answer") >= b.answer {
				continue
			}
			resp := service.DoseResponse{Status: model.DoseTaken}
			if s.chance(d.ID, "take") >= b.take {
				resp = service.DoseResponse{Status: model.DoseSkipped, Reason: []string{"Forgot to take them to work",
					"Felt nauseous", "Ran out; collecting tomorrow"}[s.rnd.Intn(3)]}
			}
			if _, err := svc.RespondToDose(s.ctx, d.ID, resp); err != nil {
				return err
			}
			s.appAnswers++
		}
	}
	return nil
}

func (s *seeder) report() {
	fmt.Printf("\nDemo practice created\n")
	fmt.Printf("  practice code : %s\n", s.opt.slug)
	fmt.Printf("  name          : %s\n", s.opt.name)
	fmt.Printf("  patients      : %d\n", len(s.patients))
	fmt.Printf("  history       : %d days of reminders: %d messages, %d WhatsApp replies, %d answers in the app\n\n",
		s.opt.days, s.sent, s.replies, s.appAnswers)
	fmt.Printf("Sign-ins (shown once):\n")
	for _, l := range s.logins {
		fmt.Printf("  %-34s %-36s %s\n", l.who, l.email, l.password)
	}
	fmt.Printf("\nThis is a demo practice. Its patients, ID numbers and phone numbers are made up, so it never\n")
	fmt.Printf("sends a message, whatever the server is configured with. Two-step verification is required for\n")
	fmt.Printf("staff when MES_REQUIRE_MFA is on; set it up at first sign-in.\n")
}
