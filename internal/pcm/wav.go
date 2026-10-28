package pcm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// WriteWAV writes data, mono s16 at rate, as a WAV file.
func WriteWAV(w io.Writer, data []byte, rate int) error {
	n := uint32(len(data) &^ 1)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 36+n)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*BytesPerSample))
	binary.LittleEndian.PutUint16(h[32:], BytesPerSample)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], n)
	if _, err := w.Write(h); err != nil {
		return err
	}
	_, err := w.Write(data[:n])
	return err
}

// ReadWAV reads a WAV file of mono s16 and returns its samples and rate.
// Chunks other than fmt and data are skipped.
func ReadWAV(r io.Reader) (data []byte, rate int, err error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("not a WAV file")
	}
	gotFmt := false
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8 : min(p+8+size, len(b))]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return nil, 0, errors.New("WAV fmt chunk too short")
			}
			format, channels := binary.LittleEndian.Uint16(body[0:]), binary.LittleEndian.Uint16(body[2:])
			bits := binary.LittleEndian.Uint16(body[14:])
			if format != 1 || channels != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("WAV is format %d, %d channels, %d bits: want PCM, mono, 16 bits", format, channels, bits)
			}
			rate, gotFmt = int(binary.LittleEndian.Uint32(body[4:])), true
		case "data":
			if !gotFmt {
				return nil, 0, errors.New("WAV data before fmt")
			}
			return body[:len(body)&^1], rate, nil
		}
		p += 8 + size + size&1 // chunks are padded to even sizes
	}
	return nil, 0, errors.New("WAV has no data chunk")
}
