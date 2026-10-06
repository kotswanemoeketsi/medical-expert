# WhatsApp reminders: setting up

The server sends medication reminders and recall messages through the
WhatsApp Cloud API, Meta's hosted API for WhatsApp Business. This guide
covers what to set up at Meta, what to put in `.env`, and how a practice
switches messaging on.

Nothing is sent until all three of these are true:

1. the server has a WhatsApp account configured (section 3);
2. Meta has approved the three message templates (section 2);
3. a practice administrator has switched messaging on for the practice
   (section 4).

Without them, everything else in the system works and no message leaves
it.

## 1. At Meta

You need a Meta Business account, and in the Meta developer dashboard an
app with the WhatsApp product added. Meta's own getting-started pages walk
through this and change from time to time, so they are the reference. You
come away with four values:

| Value | Where Meta shows it | Goes into |
|---|---|---|
| Phone number ID | WhatsApp, API setup | `MES_WHATSAPP_PHONE_NUMBER_ID` |
| A permanent access token | Business settings: a system user with the `whatsapp_business_messaging` permission | `MES_WHATSAPP_ACCESS_TOKEN` |
| App secret | App settings, Basic | `MES_WHATSAPP_APP_SECRET` |
| Verify token | You make this up: any long random string | `MES_WHATSAPP_VERIFY_TOKEN` |

Use a system user's token. The temporary token on the API setup page lasts
a day, and every reminder after that would fail.

Meta's test number can message up to five numbers you register with it.
That is enough to try everything before the practice's own number is
approved.

## 2. The message templates

WhatsApp only lets a business start a conversation with an approved
template, so free text cannot be sent even by mistake. Create these three
in WhatsApp Manager, under message templates. Use the category **Utility**
and the language **English** (code `en`; if you choose another, set
`MES_WHATSAPP_LANGUAGE` to its code). The names and the wording must match
exactly. The wording below is also what staff see as the message preview.

**`mes_dose_reminder`**

> Hi {{1}}, it's time for your {{2}} medicine. Reply TAKEN once you've taken it, or SKIP if you won't be taking it. Reply STOP to stop these reminders.

- {{1}} is the patient's first name (sample: Thabo); {{2}} is the dose
  time (sample: 08:00).
- Two quick-reply buttons, in this order: **Taken**, **Skip**.

**`mes_dose_follow_up`**

> Hi {{1}}, we haven't heard about your {{2}} medicine yet. Reply TAKEN if you've taken it, or SKIP if you won't be taking it. Reply STOP to stop these reminders.

- The same parameters and the same two buttons, in the same order.

**`mes_recall`**

> Hi {{1}}, {{2}} would like you to come in for a check-up. Please call {{3}} to make an appointment. Reply STOP to stop these messages.

- {{1}} is the first name, {{2}} the practice's name (sample: Greenside
  Clinic), {{3}} the practice's phone number (sample: 011 555 0123).
- No buttons.

Approval usually takes minutes to a day. A template Meta rejects, pauses
or disables makes every send fail with error 132001 or 132015. The
practice's messaging health shows this (section 6).

**What the templates deliberately leave out.** No medicine, condition or
result is ever named. "Time for your Metformin" tells anyone holding the
phone that its owner is diabetic. Before every send the server checks the
message against a list of health words and against every medicine name in
the catalogue, including each medicine of a combination product, and
refuses it if anything matches. A patient's first name
is checked only against the health words, because some brand names are
also people's names.

## 3. The server

In `.env`:

```
MES_WHATSAPP_PHONE_NUMBER_ID=1098765432101234
MES_WHATSAPP_ACCESS_TOKEN=EAAG...
MES_WHATSAPP_APP_SECRET=0123456789abcdef0123456789abcdef
MES_WHATSAPP_VERIFY_TOKEN=a-long-random-string-you-made-up
```

Restart the server. It logs `whatsapp messaging available` and starts the
reminder job, which runs every minute. In production it refuses to start
with an access token but no app secret. Without the secret, patients'
replies could not be verified and would all be refused.

### The webhook

Delivery receipts and patients' replies arrive at:

```
https://your-server/api/v1/webhooks/whatsapp
```

In the Meta dashboard, under WhatsApp configuration, set that as the
callback URL and give it the same verify token as
`MES_WHATSAPP_VERIFY_TOKEN`. Meta calls the URL once to check the token.
Then subscribe to the **messages** webhook field.

The URL must be reachable from the internet over https. On a practice's own
network that means a reverse proxy with a certificate, or a tunnel. Every
webhook's signature is checked against the app secret before its body is
read; one that does not match is refused with 401.

## 4. Switching it on for a practice

A practice administrator does this, because messaging patients is a step a
practice takes deliberately. With PowerShell (see [WINDOWS.md](WINDOWS.md)
for the `Api` helper):

```powershell
Api PATCH /practice @{ whatsapp_enabled = $true; phone = '011 555 0123' }
(Api GET /practice).messaging
```

`messaging.active` should now be `True`. The practice's phone number is
needed for recalls, which ask the patient to phone it. If the practice's
name says what it treats or names a medicine, for example "Greenside HIV
Clinic", switching messaging on is refused until you set a neutral name for
messages:

```powershell
Api PATCH /practice @{ messaging_name = 'Greenside Rooms'; whatsapp_enabled = $true }
```

`follow_up_minutes` sets the practice's follow-up delay: 0 for none, or -1
for the server's default (`MES_REMINDER_FOLLOW_UP`).

## 5. Who gets reminders

A patient is reminded when all of these hold:

- their preferred channel is WhatsApp and they have a mobile number;
- they have consented to reminder communications (`POST
  /patients/{id}/consents` with `purpose = 'reminder_communications'`);
- they are an active patient with an active regimen.

Doses are only scheduled for patients who can answer: these patients, and
patients with a login of their own, who answer in the app. Recording the
consent schedules the week ahead at once.

**Tell patients what reminders are, when they consent.** The requirements
ask that reminders are presented as an aid, not a guarantee (BR-4.12).
Phones are off, numbers change, and messages are delayed.

A patient who prefers SMS gets nothing, rather than being switched to
WhatsApp without being asked: they may have chosen SMS because they have
no smartphone. Only WhatsApp is built.

## 6. Day to day

**What goes out.** Each minute, every dose that has come due gets a
reminder, one message per patient per time. If the doses are still
unanswered, one follow-up goes out an hour later. A reminder that cannot go
out within an hour of its time is not sent at all. Each practice can send
`MES_NOTIFICATION_DAILY_LIMIT` messages a day (1000 by default).

**What comes back.**

| The patient | The system |
|---|---|
| Presses Taken or Skip | Answers exactly the doses that message asked about |
| Types TAKEN, Done, SKIP, Not taken, ... | Answers the doses of the message they replied to, or of the latest message to their number |
| Types STOP | Withdraws their consent to reminders, recorded as their own decision, and stops future reminders |
| Types anything else | Changes nothing. The message is kept, and staff see it in the patient's message history |
| Answers after the dose was closed | Changes nothing; the dose stays as recorded |

Only whole answers are read, word for word. "I'm not sure, can I take it
later?" is not guessed at: guessing would put a confident wrong entry into
a clinical record. Replies from a number other than the one the reminder
went to are ignored.

**Sending by hand.** A clinician can remind a patient now
(`POST /patients/{id}/notifications/reminder`) about doses that are due.
Any staff member can send a recall (`POST /patients/{id}/notifications/recall`),
typically from a recall list, at most once a week per patient.

**When things fail.** `GET /notifications/health` shows the last 24 hours
and 7 days by status, the last error, and how much of today's limit is
used, with no patient identifiers. `GET /patients/{id}/notifications` shows
each message, its status and error, and every reply. Common errors:

| Code | Meaning |
|---|---|
| 131026 | The number is not on WhatsApp, or cannot receive messages. Not retried; no follow-up is sent |
| 132001 | The template does not exist under that name and language |
| 132000, 132012 | The template's parameters do not match what was approved |
| 132015 | The template is paused |
| 190 | The access token has expired or been revoked |
| 130429, 131048, 131056 | Rate limits. Retried a minute, then five minutes, later |
| `clinical_detail` | Refused by the server before sending: see section 2 |
| `expired` | Could not be sent within the hour, so it was dropped |

## 7. Demo practices

A practice created by `cmd/seed` is marked as a demo. Its patients, ID
numbers and phone numbers are made up, so it never sends a message, and
messaging cannot be switched on for it, whatever the server is configured
with.
