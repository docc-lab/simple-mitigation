package hc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultListenerAddr is where the HC Listener accepts registrations
// (Listener/listener.cpp SERVER_PORT).
const DefaultListenerAddr = "127.0.0.1:10101"

// ErrNAK is returned when the Listener answered "NAK": it parsed the message
// but could not act on it (e.g. no task found in a Secondary's cgroup).
var ErrNAK = errors.New("hc: listener replied NAK")

// PrimaryConfig is the per-Primary tuning the Listener copies into the shared
// idleStats segment. Field meanings follow HC's Balancer (Balancer/main.c).
type PrimaryConfig struct {
	CPUs              []int
	ACBF              float64 // LOWIDLEFREQ_THRESHOLD
	LowTIC            int
	TargetIdleCores   int
	MinSecondaryCores int
}

// PrimaryMsg builds the Primary registration. Every value is a string: the
// Listener reads them with nlohmann `std::string x = json[...]`, which throws
// a type_error on a JSON number and drops the message.
func PrimaryMsg(p PrimaryConfig) map[string]string {
	tic := strconv.Itoa(p.TargetIdleCores)
	return map[string]string{
		"ctr_type":               "Primary",
		"cpuList":                FormatCPUList(p.CPUs),
		"LOWIDLEFREQ_THRESHOLD":  strconv.FormatFloat(p.ACBF, 'f', -1, 64),
		"LOW_TIC":                strconv.Itoa(p.LowTIC),
		"targetIdleCores":        tic,
		"static_targetIdleCores": tic,
		"minSecondaryCores":      strconv.Itoa(p.MinSecondaryCores),
	}
}

// SecondaryMsg registers every container of the pod with this UID as a
// Secondary. parent=any lets the Listener use the first non-pause task in
// each container instead of requiring a process named harvest_launcher, so
// unmodified aggressor images work.
func SecondaryMsg(podUID string) map[string]string {
	return map[string]string{
		"ctr_type": "Secondary",
		"pod_id":   "pod" + podUID,
		"parent":   "any",
	}
}

// ResetMsg clears all registered Primaries/Secondaries and pauses the
// Balancer (balancerControl=0).
func ResetMsg() map[string]string {
	return map[string]string{"ctr_type": "Reset"}
}

// listenerClient speaks the Listener's one-message-per-connection protocol:
// write a JSON object, half-close, read the 4-byte "ACK\0"/"NAK\0" reply.
// Half-closing matters: the Listener reads until EOF or a 1 s receive
// timeout, so without it every message would cost a second.
type listenerClient struct {
	addr    string
	timeout time.Duration
}

func (c listenerClient) send(ctx context.Context, msg map[string]string) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("hc: dial listener %s: %w", c.addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := conn.Write(body); err != nil {
		return fmt.Errorf("hc: write %s: %w", msg["ctr_type"], err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			return fmt.Errorf("hc: half-close: %w", err)
		}
	}
	buf := make([]byte, 4)
	n, err := io.ReadFull(conn, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("hc: read reply to %s: %w", msg["ctr_type"], err)
	}
	reply := strings.TrimRight(string(buf[:n]), "\x00")
	switch reply {
	case "ACK":
		return nil
	case "NAK":
		return fmt.Errorf("%w (%s)", ErrNAK, msg["ctr_type"])
	}
	return fmt.Errorf("hc: unexpected reply %q to %s", reply, msg["ctr_type"])
}
