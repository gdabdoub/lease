package lease

import (
	"errors"
	"time"
)

var (
	// ErrTokenNotMatch and ErrLeaseNotHeld could be returns only on the Update() call.
	//
	// If the concurrency token of the passed-in lease doesn't match the
	// concurrency token of the authoritative lease, it means the lease was
	// lost and regained between when the caller acquired his concurrency
	// token and when the caller called update.
	ErrTokenNotMatch = errors.New("leaser: concurrency token doesn't match the authoritative lease")
	// ErrLeaseNotHeld error will be returns only if the passed-in lease object
	// does not held be this  worker.
	ErrLeaseNotHeld = errors.New("leaser: worker does not hold the passed-in lease object")
)

// Lease type contains data pertianing to a Lease.
// Distributed systems may use leases to partition work across a fleet of workers.
// Each unit of work/task identified by a leaseKey and has a corresponding Lease.
// Every worker will contend for all leases - only one worker will successfully take each one.
// The worker should hold the lease until it is ready to stop processing the corresponding unit of work,
// or until it fails.
// When the worker stops holding the lease, another worker will take and hold the lease.
type Lease struct {
	Key     string `dynamodbav:"leaseKey"`
	Owner   string `dynamodbav:"leaseOwner"`
	Counter int    `dynamodbav:"leaseCounter"`

	// lastRenewal is used by LeaseTaker to track the last time a lease counter was incremented.
	// It is deliberately not persisted in DynamoDB.
	lastRenewal time.Time
	// concurrencyToken is used to prevent updates to leases that we have lost and re-acquired.
	// It is deliberately not persisted in DynamoDB.
	concurrencyToken string
	// extrafields holds all the fields that not belong to this package.
	extrafields map[string]interface{}
}

// NewLease gets a key(represents the lease key/name) and returns a new Lease object.
func NewLease(key string) Lease {
	return Lease{Key: key}
}

// Set extra field to the Lease object before you create or update it
// using the Leaser.
func (l *Lease) Set(key string, val interface{}) {
	if l.extrafields == nil {
		l.extrafields = make(map[string]interface{})
	}
	l.extrafields[key] = val
}

// Get extra field from the Lease object that not belongs to this package.
func (l *Lease) Get(key string) (interface{}, bool) {
	val, ok := l.extrafields[key]
	return val, ok
}

// isExpired test if the lease renewal is expired from the given time.
func (l *Lease) isExpired(t time.Duration) bool {
	return time.Since(l.lastRenewal) > t
}

// hasNoOwner return true if the current owner is null.
func (l *Lease) hasNoOwner() bool {
	return l.Owner == "NULL" || l.Owner == ""
}

// Leaser is the interface that wraps the Coordinator methods.
type Leaser interface {
	Stop()
	Start() error
	GetLeases() []Lease
	Delete(Lease) error
	Create(Lease) (Lease, error)
	Update(Lease) (Lease, error)
}
