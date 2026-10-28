package pcm

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWAVRoundTrip(t *testing.T) {
	data := tone(1000, 12000)
	var buf bytes.Buffer
	if err := WriteWAV(&buf, data, 24000); err != nil {
		t.Fatal(err)
	}
	got, rate, err := ReadWAV(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 24000 || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes at %d Hz, want %d at 24000", len(got), rate, len(data))
	}
}

func TestReadWAVSkipsOtherChunks(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteWAV(&buf, tone(10, 100), 16000); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	// Put an odd-sized LIST chunk (padded to even) between fmt and data.
	list := []byte("LIST\x03\x00\x00\x00abc\x00")
	withList := append(append(append([]byte{}, b[:36]...), list...), b[36:]...)
	binary.LittleEndian.PutUint32(withList[4:], uint32(len(withList)-8))
	got, rate, err := ReadWAV(bytes.NewReader(withList))
	if err != nil {
		t.Fatal(err)
	}
	if rate != 16000 || len(got) != 20 {
		t.Fatalf("got %d bytes at %d Hz, want 20 at 16000", len(got), rate)
	}
}

func TestReadWAVRejectsStereo(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteWAV(&buf, tone(10, 100), 16000); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	binary.LittleEndian.PutUint16(b[22:], 2)
	if _, _, err := ReadWAV(bytes.NewReader(b)); err == nil {
		t.Fatal("a stereo WAV was accepted")
	}
}
