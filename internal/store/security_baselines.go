package store

import "strings"

// Baseline is a published set of security values an administrator can start
// from. Applying one fills in the form; nothing changes until it is saved.
// Only the STIG is ever enforced (JANUS_STIG_PROFILE); the others are
// starting points.
type Baseline struct {
	ID string `json:"id"`
	// Name is what the picker shows.
	Name string `json:"name"`
	// Summary explains, in plain words, who it is for and what it sets.
	Summary string `json:"summary"`
	// Values is the current policy with this baseline applied.
	Values SecurityPolicy `json:"values"`
	// Gaps lists where the current policy falls short of it.
	Gaps []string `json:"gaps"`
}

// FederalBannerText is a general U.S. Government system-use notice (NIST
// 800-53 AC-8) for systems outside the DoD, which use DoDBannerText.
const FederalBannerText = "You are accessing a U.S. Government information system. Use of this system may be monitored, recorded, and subject to audit.\n\n" +
	"Unauthorized use of this system is prohibited and may be subject to criminal and civil penalties.\n\n" +
	"Use of this system indicates consent to monitoring and recording."

type baselineDef struct {
	id, name, summary string
	apply             func(SecurityPolicy) SecurityPolicy
	gaps              func(SecurityPolicy) []string
}

var baselineDefs = []baselineDef{
	{
		id:   "nist-800-63b",
		name: "NIST SP 800-63B (modern commercial)",
		summary: "NIST's digital identity guideline (Rev. 4, AAL2), a sensible default for most organizations. " +
			"Long passwords instead of complexity rules and forced changes, which NIST says not to require: " +
			"15-character minimum, no expiry, 1-hour idle sign-out, 24-hour sessions.",
		apply: func(p SecurityPolicy) SecurityPolicy {
			p.Sessions.IdleMinutesUser = minPos(p.Sessions.IdleMinutesUser, 60)
			p.Sessions.IdleMinutesAdmin = minPos(p.Sessions.IdleMinutesAdmin, 60)
			p.Sessions.AbsoluteMinutes = minPos(p.Sessions.AbsoluteMinutes, 1440)
			p.Lockout.MaxFailures = minPos(p.Lockout.MaxFailures, 100)
			pw := &p.Passwords
			pw.MinLength = max(pw.MinLength, 15)
			pw.RequireUpper, pw.RequireLower, pw.RequireDigit, pw.RequireSpecial = false, false, false, false
			pw.MinChangedChars, pw.MinAgeHours, pw.MaxAgeDays = 0, 0, 0
			return p
		},
		gaps: func(p SecurityPolicy) []string {
			var out []string
			add := gapAdder(&out)
			add(p.Sessions.IdleMinutesUser > 60 || p.Sessions.IdleMinutesAdmin > 60, "sessions: idle sign-out should be 1 hour or less")
			add(p.Sessions.AbsoluteMinutes > 1440, "sessions: sign in again at least every 24 hours")
			add(p.Lockout.MaxFailures > 100, "lockout: limit failed attempts to 100 or fewer")
			add(p.Passwords.MinLength < 15, "passwords: minimum length 15 when a password is the only factor")
			pw := p.Passwords
			add(pw.RequireUpper || pw.RequireLower || pw.RequireDigit || pw.RequireSpecial, "passwords: composition rules should not be required")
			add(pw.MaxAgeDays > 0, "passwords: periodic changes should not be forced")
			return out
		},
	},
	{
		id:   "pci-dss-4",
		name: "PCI DSS 4.0 (payment card data)",
		summary: "For systems that store, process or reach cardholder data. 15-minute idle sign-out, lockout after " +
			"10 failures for at least 30 minutes, 12-character passwords with letters and numbers, the last 4 " +
			"passwords cannot be reused, and changes every 90 days (passwords are the only factor here unless " +
			"people turn on two-step sign-in).",
		apply: func(p SecurityPolicy) SecurityPolicy {
			p.Sessions.IdleMinutesUser = minPos(p.Sessions.IdleMinutesUser, 15)
			p.Sessions.IdleMinutesAdmin = minPos(p.Sessions.IdleMinutesAdmin, 15)
			l := &p.Lockout
			l.MaxFailures = minPos(l.MaxFailures, 10)
			if l.LockMinutes != 0 {
				l.LockMinutes = max(l.LockMinutes, 30)
			}
			pw := &p.Passwords
			pw.MinLength = max(pw.MinLength, 12)
			pw.RequireDigit = true
			if !pw.RequireUpper && !pw.RequireLower {
				pw.RequireLower = true
			}
			pw.History = max(pw.History, 4)
			pw.MaxAgeDays = minPos(pw.MaxAgeDays, 90)
			return p
		},
		gaps: func(p SecurityPolicy) []string {
			var out []string
			add := gapAdder(&out)
			add(p.Sessions.IdleMinutesUser > 15 || p.Sessions.IdleMinutesAdmin > 15, "sessions: idle sign-out must be 15 minutes or less (8.2.8)")
			add(p.Lockout.MaxFailures > 10, "lockout: lock after no more than 10 failed attempts (8.3.4)")
			add(p.Lockout.LockMinutes != 0 && p.Lockout.LockMinutes < 30, "lockout: stay locked at least 30 minutes or until an administrator unlocks (8.3.4)")
			pw := p.Passwords
			add(pw.MinLength < 12 || !pw.RequireDigit || (!pw.RequireUpper && !pw.RequireLower), "passwords: at least 12 characters with letters and numbers (8.3.6)")
			add(pw.History < 4, "passwords: the last 4 passwords may not be reused (8.3.7)")
			add(pw.MaxAgeDays == 0 || pw.MaxAgeDays > 90, "passwords: change at least every 90 days when a password is the only factor (8.3.9)")
			return out
		},
	},
	{
		id:   "fedramp-moderate",
		name: "FedRAMP Moderate (U.S. federal cloud)",
		summary: "NIST 800-53 Moderate as FedRAMP sets it, for services used by federal agencies outside the DoD. " +
			"15-minute idle sign-out, lockout after 3 failures in 15 minutes for 30 minutes, and a U.S. Government " +
			"sign-in notice. Passwords follow NIST 800-63B: 15 characters.",
		apply: func(p SecurityPolicy) SecurityPolicy {
			p.Sessions.IdleMinutesUser = minPos(p.Sessions.IdleMinutesUser, 15)
			p.Sessions.IdleMinutesAdmin = minPos(p.Sessions.IdleMinutesAdmin, 15)
			if !p.Banner.Enabled || strings.TrimSpace(p.Banner.Text) == "" {
				p.Banner = BannerPolicy{Enabled: true, Title: "U.S. Government Information System", Text: FederalBannerText, Button: firstNonEmpty(p.Banner.Button, "I acknowledge")}
			}
			l := &p.Lockout
			l.MaxFailures = minPos(l.MaxFailures, 3)
			l.WindowMinutes = max(l.WindowMinutes, 15)
			if l.LockMinutes != 0 {
				l.LockMinutes = max(l.LockMinutes, 30)
			}
			p.Passwords.MinLength = max(p.Passwords.MinLength, 15)
			return p
		},
		gaps: func(p SecurityPolicy) []string {
			var out []string
			add := gapAdder(&out)
			add(p.Sessions.IdleMinutesUser > 15 || p.Sessions.IdleMinutesAdmin > 15, "sessions: idle sign-out must be 15 minutes or less (AC-11)")
			add(!p.Banner.Enabled || strings.TrimSpace(p.Banner.Text) == "", "banner: a system use notice must be shown before sign-in (AC-8)")
			add(p.Lockout.MaxFailures > 3 || p.Lockout.WindowMinutes < 15, "lockout: lock after 3 failed attempts in 15 minutes (AC-7)")
			add(p.Lockout.LockMinutes != 0 && p.Lockout.LockMinutes < 30, "lockout: stay locked at least 30 minutes (AC-7)")
			add(p.Passwords.MinLength < 15, "passwords: minimum length 15 (IA-5, per NIST 800-63B)")
			return out
		},
	},
	{
		id:   "dod-stig",
		name: "DoD STIG (defense)",
		summary: "The Security Technical Implementation Guide is the security checklist the U.S. Department of War (DoD) " +
			"requires for its software. Outside defense work it is simply the strictest of these: 15/10-minute idle " +
			"sign-out, 3 sessions per person, the DoD notice, lockout after 3 failures until an administrator unlocks, " +
			"and 15-character complex passwords that expire after 180 days.",
		apply: STIGSecurityPolicy,
		gaps:  STIGFindings,
	},
}

func gapAdder(out *[]string) func(bool, string) {
	return func(bad bool, msg string) {
		if bad {
			*out = append(*out, msg)
		}
	}
}

// SecurityBaselines returns every baseline applied to p, with p's gaps.
func SecurityBaselines(p SecurityPolicy) []Baseline {
	out := make([]Baseline, 0, len(baselineDefs))
	for _, d := range baselineDefs {
		v := d.apply(p)
		v.normalize()
		g := d.gaps(p)
		if g == nil {
			g = []string{}
		}
		out = append(out, Baseline{ID: d.id, Name: d.name, Summary: d.summary, Values: v, Gaps: g})
	}
	return out
}
