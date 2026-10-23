package body

import "bytes"

// HeadAndTail collects output while keeping memory bounded, holding onto
// the beginning and the end. A build log's first lines say what ran and its
// last lines say why it failed; the middle is filler.
type HeadAndTail struct {
	headMax int
	tailMax int
	head    [][]byte
	headLen int
	tail    [][]byte
	tailLen int
	total   int
}

// NewHeadAndTail creates a bounded output collector.
func NewHeadAndTail(max int) *HeadAndTail {
	headMax := max * 3 / 10
	return &HeadAndTail{headMax: headMax, tailMax: max - headMax}
}

// Push adds one output chunk; the bytes are copied.
func (h *HeadAndTail) Push(chunk []byte) {
	h.total += len(chunk)
	if h.headLen < h.headMax {
		take := min(len(chunk), h.headMax-h.headLen)
		h.head = append(h.head, append([]byte{}, chunk[:take]...))
		h.headLen += take
		chunk = chunk[take:]
		if len(chunk) == 0 {
			return
		}
	}
	h.tail = append(h.tail, append([]byte{}, chunk...))
	h.tailLen += len(chunk)
	for len(h.tail) > 1 && h.tailLen-len(h.tail[0]) >= h.tailMax {
		h.tailLen -= len(h.tail[0])
		h.tail = h.tail[1:]
	}
}

func trimToCharStart(buf []byte) []byte {
	i := 0
	for i < len(buf) && i < 4 && buf[i]&0xc0 == 0x80 {
		i++
	}
	return buf[i:]
}

// Finish returns the kept bytes and how many middle bytes were dropped.
func (h *HeadAndTail) Finish() (buf []byte, dropped int) {
	head := bytes.Join(h.head, nil)
	tail := bytes.Join(h.tail, nil)
	if len(tail) > h.tailMax {
		tail = trimToCharStart(tail[len(tail)-h.tailMax:])
	}
	dropped = h.total - len(head) - len(tail)
	if dropped <= 0 {
		return append(head, tail...), 0
	}
	note := []byte(string([]byte{10}) + "…[" + itoa(dropped) + " bytes dropped from the middle]…" + string([]byte{10}))
	return append(append(head, note...), tail...), dropped
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
