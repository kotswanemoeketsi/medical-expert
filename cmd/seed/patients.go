package main

import (
	"github.com/yourorg/medical-expert-system/internal/model"
	"github.com/yourorg/medical-expert-system/internal/service"
)

// behaviour is how a simulated patient answers reminders.
type behaviour struct {
	answer   float64 // chance of answering a reminder at all
	take     float64 // chance an answer is "taken" rather than "skip"
	typed    float64 // chance of typing the answer instead of pressing a button
	minDelay int     // minutes after the reminder
	maxDelay int
}

var profiles = map[string]behaviour{
	"reliable": {answer: 0.96, take: 0.97, typed: 0.25, minDelay: 1, maxDelay: 25},
	"good":     {answer: 0.85, take: 0.9, typed: 0.3, minDelay: 2, maxDelay: 50},
	"skipper":  {answer: 0.9, take: 0.55, typed: 0.2, minDelay: 1, maxDelay: 30},
	"patchy":   {answer: 0.5, take: 0.85, typed: 0.35, minDelay: 5, maxDelay: 110},
	"silent":   {answer: 0.05, take: 0.8, typed: 0.5, minDelay: 30, maxDelay: 180},
}

type demoCondition struct {
	code, name string
	chronic    bool
}

type demoMed struct {
	medicine     string // generic name and strength, as in the catalogue
	dose         string
	frequency    model.Frequency
	instructions string
}

type demoLab struct {
	ago     int // days before the end
	panel   string
	items   [][5]string // analyte, value, unit, range, flag
	ackNote string      // empty: not yet acknowledged
}

type scriptPlan struct {
	repeats   int
	repeatsAt []int // days before the end
}

type demoPatient struct {
	first, last string
	sex         model.Sex
	dob         model.Date
	channel     model.Channel
	reminders   bool // consented to reminder messages
	ai          bool
	portal      bool // has a login of their own
	badNumber   bool // the number is not on WhatsApp
	draftNote   bool
	profile     string
	waking      *[2]string
	conditions  []demoCondition
	allergies   [][3]string
	meds        []demoMed
	reviews     []int // signed reviews, days before the end, oldest first
	script      *scriptPlan
	labs        []demoLab

	id       model.ID
	e164     string
	actor    service.Actor
	regimens []model.ID
}

var (
	diabetes       = demoCondition{"E11.9", "Type 2 diabetes mellitus", true}
	hypertension   = demoCondition{"I10", "Essential hypertension", true}
	hyperlipidemia = demoCondition{"E78.5", "Hyperlipidaemia", true}
	hypothyroidism = demoCondition{"E03.9", "Hypothyroidism", true}
	asthma         = demoCondition{"J45.9", "Asthma", true}
	epilepsy       = demoCondition{"G40.9", "Epilepsy", true}
	gout           = demoCondition{"M10.9", "Gout", true}
	depression     = demoCondition{"F32.9", "Depressive episode", true}

	metformin     = demoMed{"Metformin 500 mg", "1 tablet", model.FreqTwiceDaily, "With breakfast and supper"}
	gliclazide    = demoMed{"Gliclazide 80 mg", "1 tablet", model.FreqOnceDaily, "With breakfast"}
	amlodipine    = demoMed{"Amlodipine 5 mg", "1 tablet", model.FreqOnceDaily, ""}
	enalapril     = demoMed{"Enalapril 10 mg", "1 tablet", model.FreqTwiceDaily, ""}
	hctz          = demoMed{"Hydrochlorothiazide 12.5 mg", "1 tablet", model.FreqOnceDaily, "In the morning"}
	atorvastatin  = demoMed{"Atorvastatin 20 mg", "1 tablet", model.FreqOnceDaily, "At night"}
	levothyroxine = demoMed{"Levothyroxine 50 mcg", "1 tablet", model.FreqOnceDaily, "On an empty stomach"}
	carbamazepine = demoMed{"Carbamazepine 200 mg", "1 tablet", model.FreqTwiceDaily, ""}
	budesonide    = demoMed{"Budesonide 200 mcg/dose", "2 puffs", model.FreqTwiceDaily, "Rinse the mouth afterwards"}
	salbutamol    = demoMed{"Salbutamol 100 mcg/dose", "2 puffs", model.FreqAsNeeded, "When short of breath"}
	allopurinol   = demoMed{"Allopurinol 100 mg", "1 tablet", model.FreqOnceDaily, "After a meal"}
	fluoxetine    = demoMed{"Fluoxetine 20 mg", "1 capsule", model.FreqOnceDaily, "In the morning"}
)

func hba1c(ago int, value, flag, ack string) demoLab {
	return demoLab{ago: ago, panel: "HbA1c", items: [][5]string{{"HbA1c", value, "%", "< 7.0", flag}}, ackNote: ack}
}

// demoPatients are made up. So are their ID numbers and phone numbers,
// which the seeder generates.
func demoPatients() []*demoPatient {
	d := model.NewDate
	wa, none, sms := model.ChannelWhatsApp, model.ChannelNone, model.ChannelSMS
	f, m := model.SexFemale, model.SexMale
	return []*demoPatient{
		{first: "Thabo", last: "Nkosi", sex: m, dob: d(1966, 6, 16), channel: wa, reminders: true, ai: true, profile: "reliable",
			conditions: []demoCondition{diabetes, hypertension}, allergies: [][3]string{{"Penicillin", "Rash", "moderate"}},
			meds: []demoMed{metformin, gliclazide, amlodipine}, reviews: []int{250, 130, 40},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{10}},
			labs:   []demoLab{hba1c(41, "7.9", "high", "Better than last time; discussed diet.")}},
		{first: "Nomvula", last: "Dlamini", sex: f, dob: d(1972, 3, 2), channel: wa, reminders: true, profile: "good",
			conditions: []demoCondition{hypertension}, meds: []demoMed{enalapril, hctz}, reviews: []int{310, 172},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{142, 112, 82, 52}}},
		{first: "Pieter", last: "van der Merwe", sex: m, dob: d(1958, 11, 21), channel: wa, reminders: true, profile: "patchy",
			conditions: []demoCondition{hyperlipidemia, hypertension}, meds: []demoMed{atorvastatin, amlodipine},
			reviews: []int{260, 75}, script: &scriptPlan{repeats: 5, repeatsAt: []int{45, 15}},
			labs: []demoLab{{ago: 76, panel: "Lipid profile", items: [][5]string{
				{"Total cholesterol", "5.9", "mmol/L", "< 5.0", "high"}, {"LDL cholesterol", "3.8", "mmol/L", "< 3.0", "high"}},
				ackNote: "Statin started; repeat in three months."}}},
		{first: "Ayesha", last: "Patel", sex: f, dob: d(1980, 8, 9), channel: wa, reminders: true, ai: true, profile: "reliable",
			conditions: []demoCondition{hypothyroidism}, meds: []demoMed{levothyroxine}, reviews: []int{190, 60},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{30}},
			labs: []demoLab{{ago: 61, panel: "Thyroid function", items: [][5]string{{"TSH", "2.1", "mIU/L", "0.4 - 4.0", "none"}},
				ackNote: "Normal."}}},
		{first: "Sipho", last: "Mahlangu", sex: m, dob: d(1975, 1, 30), channel: wa, reminders: true, profile: "silent",
			draftNote: true, conditions: []demoCondition{diabetes}, meds: []demoMed{metformin}, reviews: []int{300, 230},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{200, 170, 140, 110, 80}},
			labs:   []demoLab{hba1c(3, "11.2", "high", "")}},
		{first: "Lindiwe", last: "Zulu", sex: f, dob: d(1991, 4, 12), channel: wa, reminders: true, profile: "good",
			conditions: []demoCondition{asthma}, meds: []demoMed{budesonide, salbutamol}, reviews: []int{120, 25},
			script: &scriptPlan{repeats: 3}},
		{first: "Johan", last: "Botha", sex: m, dob: d(1962, 9, 3), channel: wa, reminders: true, profile: "skipper",
			conditions: []demoCondition{gout, hypertension}, allergies: [][3]string{{"Aspirin", "Wheezing", "severe"}},
			meds: []demoMed{allopurinol, amlodipine}, reviews: []int{150, 20}, script: &scriptPlan{repeats: 5}},
		{first: "Zanele", last: "Khumalo", sex: f, dob: d(1985, 12, 1), channel: wa, reminders: true, profile: "reliable",
			waking: &[2]string{"20:00", "08:00"}, conditions: []demoCondition{epilepsy}, meds: []demoMed{carbamazepine},
			reviews: []int{210, 90}, script: &scriptPlan{repeats: 5, repeatsAt: []int{60, 30}}},
		{first: "Rajesh", last: "Naidoo", sex: m, dob: d(1955, 5, 25), channel: wa, reminders: true, ai: true, profile: "good",
			conditions: []demoCondition{diabetes, hyperlipidemia}, meds: []demoMed{metformin, atorvastatin},
			reviews: []int{200, 95}, script: &scriptPlan{repeats: 2, repeatsAt: []int{65, 35}},
			labs: []demoLab{hba1c(96, "7.2", "high", "Close to target.")}},
		{first: "Busisiwe", last: "Mthembu", sex: f, dob: d(1990, 2, 14), channel: wa, reminders: true, profile: "patchy",
			conditions: []demoCondition{depression}, meds: []demoMed{fluoxetine}, reviews: []int{100, 45},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{15}}},
		{first: "Kagiso", last: "Molefe", sex: m, dob: d(1978, 7, 19), channel: wa, reminders: true, profile: "good",
			badNumber: true, conditions: []demoCondition{hypertension}, meds: []demoMed{amlodipine}, reviews: []int{140, 50},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{20}}},
		{first: "Elizabeth", last: "Smith", sex: f, dob: d(1948, 10, 5), channel: none, portal: true, profile: "reliable",
			conditions: []demoCondition{hypertension, hypothyroidism}, meds: []demoMed{amlodipine, levothyroxine},
			reviews: []int{180, 70}, script: &scriptPlan{repeats: 5, repeatsAt: []int{40, 10}}},
		{first: "Themba", last: "Ndlovu", sex: m, dob: d(1983, 3, 28), channel: wa, reminders: true, profile: "skipper",
			conditions: []demoCondition{asthma}, meds: []demoMed{budesonide, salbutamol}, reviews: []int{160, 35},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{5}}},
		{first: "Fatima", last: "Adams", sex: f, dob: d(1967, 6, 6), channel: wa, reminders: true, profile: "good",
			conditions: []demoCondition{diabetes}, meds: []demoMed{metformin, gliclazide}, reviews: []int{220, 110},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{80, 50, 20}},
			labs: []demoLab{hba1c(2, "8.4", "high", ""), {ago: 2, panel: "Urea and electrolytes", items: [][5]string{
				{"Sodium", "138", "mmol/L", "135 - 145", "none"}, {"Potassium", "6.3", "mmol/L", "3.5 - 5.1", "critical_high"},
				{"Creatinine", "142", "umol/L", "49 - 90", "high"}}}}},
		{first: "Mandla", last: "Sithole", sex: m, dob: d(1970, 1, 17), channel: sms, profile: "good",
			conditions: []demoCondition{hypertension}, meds: []demoMed{enalapril}, reviews: []int{240, 100},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{70, 40, 10}}},
		{first: "Precious", last: "Mokoena", sex: f, dob: d(1993, 9, 9), channel: wa, reminders: true, profile: "reliable",
			conditions: []demoCondition{hypothyroidism}, meds: []demoMed{levothyroxine}, reviews: []int{130, 30},
			script: &scriptPlan{repeats: 5},
			labs:   []demoLab{{ago: 2, panel: "Thyroid function", items: [][5]string{{"TSH", "6.8", "mIU/L", "0.4 - 4.0", "high"}}}}},
		{first: "Willem", last: "Pretorius", sex: m, dob: d(1952, 4, 2), channel: wa, reminders: false, profile: "good",
			conditions: []demoCondition{hyperlipidemia}, meds: []demoMed{atorvastatin}, reviews: []int{170, 55},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{25}}},
		{first: "Nokuthula", last: "Shabalala", sex: f, dob: d(1960, 8, 23), channel: wa, reminders: true, portal: true,
			profile: "patchy", conditions: []demoCondition{diabetes, hypertension}, meds: []demoMed{metformin, hctz},
			reviews: []int{205, 88}, script: &scriptPlan{repeats: 5, repeatsAt: []int{58, 28}}},
		{first: "Lerato", last: "Tau", sex: f, dob: d(1988, 5, 15), channel: wa, reminders: true, profile: "good",
			conditions: []demoCondition{{"J06.9", "Acute upper respiratory infection", false}}},
		{first: "David", last: "Cohen", sex: m, dob: d(1976, 11, 11), channel: wa, reminders: true, profile: "good",
			conditions: []demoCondition{gout}, meds: []demoMed{allopurinol}, reviews: []int{110, 15},
			script: &scriptPlan{repeats: 5}},
		{first: "Grace", last: "Mabuza", sex: f, dob: d(1974, 7, 7), channel: wa, reminders: true, profile: "good",
			conditions: []demoCondition{epilepsy}, meds: []demoMed{carbamazepine}, reviews: []int{195, 65},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{35, 5}}},
		{first: "Andile", last: "Cele", sex: m, dob: d(1998, 2, 20), channel: wa, reminders: true, profile: "good",
			conditions: []demoCondition{{"S93.4", "Sprained ankle", false}}},
		{first: "Hendrik", last: "Joubert", sex: m, dob: d(1964, 12, 24), channel: wa, reminders: true, profile: "silent",
			conditions: []demoCondition{diabetes}, meds: []demoMed{metformin}, reviews: []int{280},
			script: &scriptPlan{repeats: 5, repeatsAt: []int{250, 220, 190, 160, 130}}},
		{first: "Sarah", last: "Moodley", sex: f, dob: d(1982, 4, 4), channel: wa, reminders: true, ai: true, profile: "reliable",
			conditions: []demoCondition{depression}, meds: []demoMed{fluoxetine}, reviews: []int{90, 28},
			script: &scriptPlan{repeats: 5}},
	}
}
