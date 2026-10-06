// Package notify sends WhatsApp messages through the Cloud API and reads
// what comes back: delivery receipts and patients' replies.
//
// Business-initiated WhatsApp messages must use templates Meta has approved,
// so free text cannot be sent even if someone wanted to. Nothing is sent
// until the templates below are approved under these names; the wording to
// submit is in docs/WHATSAPP.md and must match Body exactly.
package notify

import (
	"fmt"
	"strconv"
	"strings"
)

// Template is a message registered with Meta.
type Template struct {
	Name string
	// Body is the approved wording, with {{1}}, {{2}}... where the
	// parameters go. It is used for the preview staff see.
	Body string
	// Params says what each placeholder holds, in order.
	Params []string
	// Buttons are quick-reply buttons, in order. Each message sets its own
	// payload per button, which is how a button press finds its reminder.
	Buttons []string
}

// The templates. Each carries a first name and a time or the practice's
// name, never a medicine, a condition or a result: "Time for your
// Metformin" tells anyone holding the phone that its owner is diabetic.
var (
	DoseReminder = Template{
		Name: "mes_dose_reminder",
		Body: "Hi {{1}}, it's time for your {{2}} medicine. Reply TAKEN once you've taken it, " +
			"or SKIP if you won't be taking it. Reply STOP to stop these reminders.",
		Params:  []string{"the patient's first name", "the dose time, such as 08:00"},
		Buttons: []string{"Taken", "Skip"},
	}
	DoseFollowUp = Template{
		Name: "mes_dose_follow_up",
		Body: "Hi {{1}}, we haven't heard about your {{2}} medicine yet. Reply TAKEN if you've taken it, " +
			"or SKIP if you won't be taking it. Reply STOP to stop these reminders.",
		Params:  []string{"the patient's first name", "the dose time, such as 08:00"},
		Buttons: []string{"Taken", "Skip"},
	}
	Recall = Template{
		Name: "mes_recall",
		Body: "Hi {{1}}, {{2}} would like you to come in for a check-up. Please call {{3}} to make " +
			"an appointment. Reply STOP to stop these messages.",
		Params: []string{"the patient's first name", "the practice's name", "the practice's phone number"},
	}
)

// Templates lists every template, for documentation and start-up checks.
var Templates = []Template{DoseReminder, DoseFollowUp, Recall}

// Preview renders the message as the patient will read it.
func (t Template) Preview(params []string) (string, error) {
	if len(params) != len(t.Params) {
		return "", fmt.Errorf("notify: template %s takes %d parameters, got %d", t.Name, len(t.Params), len(params))
	}
	out := t.Body
	for i, p := range params {
		if strings.TrimSpace(p) == "" {
			return "", fmt.Errorf("notify: template %s parameter %d (%s) is empty", t.Name, i+1, t.Params[i])
		}
		out = strings.ReplaceAll(out, "{{"+strconv.Itoa(i+1)+"}}", p)
	}
	return out, nil
}

// ByName finds a template, for resending a stored notification.
func ByName(name string) (Template, bool) {
	for _, t := range Templates {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}
