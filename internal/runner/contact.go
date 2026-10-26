package runner

import (
	"fmt"
	"time"
)

// Contact (ADR-0003, ADR-0012): schedules run without anyone watching, so
// a runner that hears nothing from its people for ContactTimeout pauses
// every schedule. Any request on the runner's socket counts (only the
// Control Center and the hcc CLI use it; agents never can), and the
// Control Center sends one regularly while it runs. Like the kill switch,
// contact does not unpause anything: each schedule is resumed on purpose.

// DefaultContactTimeout is the default for Config.ContactTimeout.
const DefaultContactTimeout = 72 * time.Hour

// contactSaveEvery throttles persisting the time of the last contact.
const contactSaveEvery = time.Minute

// Contact records that a person (or their Control Center) reached the
// runner.
func (r *Runner) Contact() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.cfg.Now()
	r.lastContact, r.contactLapsed = now, false
	if r.cfg.SaveContact != nil && now.Sub(r.savedContact) >= contactSaveEvery {
		r.savedContact = now
		if err := r.cfg.SaveContact(now); err != nil {
			r.cfg.Log.Error("time of last contact not saved", "err", err)
		}
	}
}

// checkContactLocked pauses every active schedule while contact has
// lapsed (also ones that became active since).
func (r *Runner) checkContactLocked(now time.Time) {
	t := r.cfg.ContactTimeout
	if t <= 0 || now.Sub(r.lastContact) < t {
		return
	}
	if !r.contactLapsed {
		r.cfg.Log.Warn("contact lapsed: schedules paused", "lastContact", r.lastContact, "timeout", t.String())
	}
	r.contactLapsed = true
	r.pauseAllLocked(fmt.Sprintf("paused: nobody contacted this runner for %s (no Control Center or hcc)", t))
}

// contactDeadlineLocked is when contact lapses (zero: never, or it has).
func (r *Runner) contactDeadlineLocked() time.Time {
	if r.cfg.ContactTimeout <= 0 || r.contactLapsed {
		return time.Time{}
	}
	return r.lastContact.Add(r.cfg.ContactTimeout)
}
