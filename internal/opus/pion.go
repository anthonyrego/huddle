package opus

import (
	popus "github.com/pion/opus"
)

// pionDecoder is pion/opus's decoder. It can't conceal on its own, so a
// lost frame repeats the last one heard, a quarter quieter each time.
type pionDecoder struct {
	d    popus.Decoder
	last []float32 // the last FrameSamples decoded or made up
}

func newPionDecoder() (*pionDecoder, error) {
	d, err := popus.NewDecoderWithOutput(SampleRate, 1)
	if err != nil {
		return nil, err
	}
	return &pionDecoder{d: d, last: make([]float32, FrameSamples)}, nil
}

func (d *pionDecoder) Decode(packet []byte, out []float32) (int, error) {
	if len(packet) == 0 {
		return d.Conceal(out[:FrameSamples])
	}
	n, err := d.d.DecodeToFloat32(packet, out)
	if err != nil {
		return 0, err
	}
	if n >= FrameSamples {
		copy(d.last, out[n-FrameSamples:n])
	}
	return n, nil
}

func (d *pionDecoder) DecodeFEC(_ []byte, out []float32) (int, error) {
	return d.Conceal(out[:FrameSamples])
}

func (d *pionDecoder) Conceal(out []float32) (int, error) {
	for i := range d.last {
		d.last[i] *= 0.75
	}
	for i := range out {
		out[i] = d.last[i%len(d.last)]
	}
	return len(out), nil
}

func (d *pionDecoder) HasFEC() bool { return false }

func (d *pionDecoder) Close() error { return nil }
