package agent

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/anthonyrego/huddle/internal/server/lktoken"
)

// TestAudioOnly joins LiveKit as the whiteboard while someone publishes a
// voice and a camera: the whiteboard is subscribed to the voice and not
// to the camera, is handed the voice, and counts one person. Against a
// running LiveKit only:
//
//	HUDDLE_LIVEKIT_TEST=ws://host:7880 LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret go test ./internal/server/agent -run AudioOnly -v
func TestAudioOnly(t *testing.T) {
	url := os.Getenv("HUDDLE_LIVEKIT_TEST")
	if url == "" {
		t.Skip("set HUDDLE_LIVEKIT_TEST to a LiveKit URL")
	}
	m := lktoken.Minter{Key: os.Getenv("LIVEKIT_API_KEY"), Secret: os.Getenv("LIVEKIT_API_SECRET")}
	room := fmt.Sprintf("audio-only-%06d", rand.IntN(1e6))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Someone with a voice and a camera.
	tok, err := m.Mint(lktoken.Grant{Room: room, Identity: "someone", Name: "Someone"})
	if err != nil {
		t.Fatal(err)
	}
	lk, err := lksdk.ConnectToRoomWithToken(url, tok, lksdk.NewRoomCallback(), lksdk.WithAutoSubscribe(false))
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Disconnect()
	voice, _ := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2})
	if _, err := lk.LocalParticipant.PublishTrack(voice, &lksdk.TrackPublicationOptions{Name: "microphone", Source: livekit.TrackSource_MICROPHONE}); err != nil {
		t.Fatal(err)
	}
	cam, _ := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"})
	if _, err := lk.LocalParticipant.PublishTrack(cam, &lksdk.TrackPublicationOptions{Name: "camera", Source: livekit.TrackSource_CAMERA}); err != nil {
		t.Fatal(err)
	}
	var someoneWrites writes
	go func() {
		start := time.Now()
		for n := uint32(0); ctx.Err() == nil; n++ {
			time.Sleep(time.Until(start.Add(time.Duration(n) * 20 * time.Millisecond)))
			someoneWrites.wrote(n)
			voice.WriteSample(media.Sample{Data: stamped(n), Duration: 20 * time.Millisecond}, nil)
			cam.WriteSample(media.Sample{Data: []byte{0, 0, 0, 1, 0x41, 0x9a, 0x20}, Duration: 33 * time.Millisecond}, nil)
		}
	}()

	var mu sync.Mutex
	var arrived, handed []string
	var someoneTransit durations
	mr, err := LiveKit{Minter: m, URL: url}.Join(ctx, room, Hearing{
		Arrived: func(identity, name string) { mu.Lock(); arrived = append(arrived, identity+" "+name); mu.Unlock() },
		Left:    func(string) {},
		Voice: func(identity, _ string, track VoiceTrack) {
			mu.Lock()
			handed = append(handed, identity+" "+track.Codec().MimeType)
			mu.Unlock()
			go transit(track, &someoneWrites, someoneTransit.add)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mr.Leave()
	r := mr.(*lkRoom)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var audio, video, audioSub, videoSub int
		for _, rp := range r.room.GetRemoteParticipants() {
			for _, pub := range rp.TrackPublications() {
				switch pub.Kind() {
				case lksdk.TrackKindAudio:
					audio++
					if pub.IsSubscribed() {
						audioSub++
					}
				case lksdk.TrackKindVideo:
					video++
					if pub.IsSubscribed() {
						videoSub++
					}
				}
			}
		}
		if videoSub > 0 {
			t.Fatalf("the whiteboard is subscribed to %d videos", videoSub)
		}
		mu.Lock()
		nHanded := len(handed)
		mu.Unlock()
		if audio == 1 && video == 1 && audioSub == 1 && nHanded > 0 {
			// Give a wrong subscription a moment to show up too.
			if time.Now().After(deadline.Add(-7 * time.Second)) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 10 s: %d audio (%d subscribed), %d video; voices handed over: %d", audio, audioSub, video, nHanded)
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handed) != 1 || handed[0] != "someone "+webrtc.MimeTypeOpus {
		t.Fatalf("voices handed over: %q", handed)
	}
	if len(arrived) == 0 || arrived[0] != "someone Someone" || mr.People() != 1 {
		t.Fatalf("arrived %q, %d people", arrived, mr.People())
	}
	t.Logf("voice subscribed and handed over, camera not; arrived %q", arrived)
	ds := someoneTransit.get()
	med, p95 := median(ds)
	t.Logf("someone → the whiteboard: %d frames, transit median %v, p95 %v", len(ds), med.Round(10*time.Microsecond), p95.Round(10*time.Microsecond))
}
