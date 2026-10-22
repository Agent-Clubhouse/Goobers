package journal

// InstanceStore is the scheduler-policy journal seam: append decisions and
// resume projection reads from a durable sequence watermark.
type InstanceStore interface {
	Append(Event) error
	AppendBestEffort(Event)
	ReadAfterSeq(uint64) ([]Event, error)
	LastSeq() (uint64, error)
	Dir() string
	Close() error
}

// ReadAfterSeq returns events committed after seq from this log's directory.
func (l *InstanceLog) ReadAfterSeq(seq uint64) ([]Event, error) {
	return ReadInstanceLogAfterSeq(l.dir, seq)
}

// LastSeq returns this log's current durable sequence watermark.
func (l *InstanceLog) LastSeq() (uint64, error) {
	return InstanceLogLastSeq(l.dir)
}

var _ InstanceStore = (*InstanceLog)(nil)
