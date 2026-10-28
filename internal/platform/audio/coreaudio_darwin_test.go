package audio

import "testing"

// TestCoreAudioReadsRates reads the default devices' rates as
// matchSpeakersToMic does, without changing them or opening a stream.
func TestCoreAudioReadsRates(t *testing.T) {
	if err := loadCoreAudio(); err != nil {
		t.Fatal(err)
	}
	for _, input := range []bool{true, false} {
		dev, err := defaultDevice(input)
		if err != nil {
			t.Skipf("no default device (input %v): %v", input, err)
		}
		rate, err := nominalRate(dev)
		if err != nil {
			t.Fatal(err)
		}
		rates, err := availableRates(dev)
		if err != nil {
			t.Fatal(err)
		}
		if rate <= 0 || len(rates) == 0 {
			t.Fatalf("device %d: rate %.0f, available %v", dev, rate, rates)
		}
		t.Logf("input %v: device %d at %.0f Hz, can run at %v", input, dev, rate, rates)
	}
}
