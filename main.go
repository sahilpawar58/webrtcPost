package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/ivfreader"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggreader"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

const (
	audioFileName   = "output.opus" // Ensure these paths are correct
	videoFileName   = "output.ivf"
	oggPageDuration = time.Millisecond * 20
)

func saveToDisk(i media.Writer, track *webrtc.TrackRemote) {
	defer func() {
		if err := i.Close(); err != nil {
			panic(err)
		}
	}()

	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			fmt.Println(err)
			return
		}
		if err := i.WriteRTP(rtpPacket); err != nil {
			fmt.Println(err)
			return
		}
	}
}

func isUUID(s string) bool {
	// UUIDs have a specific format, so let's check if it matches
	// Note: This is a basic check; for more robust validation, consider using a UUID library
	return len(s) == 36 && strings.Count(s, "-") == 4
}

func setupMediaTracks(peerConnection *webrtc.PeerConnection, videoFileName, audioFileName string, iceConnectedCtx context.Context) error {
	haveVideoFile := fileExists(videoFileName)
	haveAudioFile := fileExists(audioFileName)

	if !haveAudioFile && !haveVideoFile {
		return fmt.Errorf("Could not find `%s` or `%s`", audioFileName, videoFileName)
	}

	if haveVideoFile {
		if err := setupVideoTrack(peerConnection, videoFileName, iceConnectedCtx); err != nil {
			return err
		}
	}

	if haveAudioFile {
		if err := setupAudioTrack(peerConnection, audioFileName, iceConnectedCtx); err != nil {
			return err
		}
	}

	return nil
}

func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	return !os.IsNotExist(err)
}

func setupVideoTrack(peerConnection *webrtc.PeerConnection, videoFileName string, iceConnectedCtx context.Context) error {
	file, err := os.Open(videoFileName)
	if err != nil {
		return err
	}
	defer file.Close()

	_, header, err := ivfreader.NewWith(file)
	if err != nil {
		return err
	}

	var trackCodec string
	switch header.FourCC {
	case "AV01":
		trackCodec = webrtc.MimeTypeAV1
	case "VP90":
		trackCodec = webrtc.MimeTypeVP9
	case "VP80":
		trackCodec = webrtc.MimeTypeVP8
	default:
		return fmt.Errorf("Unable to handle FourCC %s", header.FourCC)
	}

	videoTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: trackCodec}, "video", "pion")
	if err != nil {
		return err
	}

	rtpSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		return err
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	go func() {
		file, err := os.Open(videoFileName)
		if err != nil {
			panic(err)
		}
		defer file.Close()

		ivf, _, err := ivfreader.NewWith(file)
		if err != nil {
			panic(err)
		}

		<-iceConnectedCtx.Done()

		ticker := time.NewTicker(time.Millisecond * time.Duration((float32(header.TimebaseNumerator)/float32(header.TimebaseDenominator))*1000))
		defer ticker.Stop()
		for ; true; <-ticker.C {
			frame, _, err := ivf.ParseNextFrame()
			if errors.Is(err, io.EOF) {
				fmt.Printf("All video frames parsed and sent")
				return
			}

			if err != nil {
				panic(err)
			}

			if err := videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); err != nil {
				panic(err)
			}
		}
	}()
	return nil
}

func setupAudioTrack(peerConnection *webrtc.PeerConnection, audioFileName string, iceConnectedCtx context.Context) error {
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return err
	}

	rtpSender, err := peerConnection.AddTrack(audioTrack)
	if err != nil {
		return err
	}

	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	go func() {
		file, err := os.Open(audioFileName)
		if err != nil {
			panic(err)
		}
		defer file.Close()

		ogg, _, err := oggreader.NewWith(file)
		if err != nil {
			panic(err)
		}

		<-iceConnectedCtx.Done()

		var lastGranule uint64
		ticker := time.NewTicker(oggPageDuration)
		defer ticker.Stop()
		for ; true; <-ticker.C {
			pageData, pageHeader, err := ogg.ParseNextPage()
			if errors.Is(err, io.EOF) {
				fmt.Printf("All audio pages parsed and sent")
				return
			}

			if err != nil {
				panic(err)
			}

			sampleCount := float64(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

			if err := audioTrack.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); err != nil {
				panic(err)
			}
		}
	}()
	return nil
}

func main() {

	app := fiber.New()

	app.Use(cors.New(cors.Config{
		AllowOrigins: "http://localhost:5173", // Allow specific origin
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
		AllowHeaders: "Origin, Content-Type, Accept",
	}))
	app.Post("/video", func(c *fiber.Ctx) error {
		var body map[string]interface{}
		if err := c.BodyParser(&body); err != nil {
			return err
		}
		base, okBase := body["base"].(string)
		if !okBase {
			return c.SendString("Parameter 'base' not found or not a string")
		}

		// Create a new RTCPeerConnection
		peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					URLs:       []string{"turn:cvcp.csinfocomm.com:3478"},
					Username:   "admin",
					Credential: "pass@123",
				},
			},
		})
		if err != nil {
			return err
		}

		iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())
		defer func() {
			if cErr := peerConnection.Close(); cErr != nil {
				fmt.Printf("cannot close peerConnection: %v\n", cErr)
			}
		}()

		if err := setupMediaTracks(peerConnection, videoFileName, audioFileName, iceConnectedCtx); err != nil {
			return err
		}

		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			fmt.Printf("Connection State has changed %s \n", connectionState.String())
			if connectionState == webrtc.ICEConnectionStateConnected {
				iceConnectedCtxCancel()
			}
		})

		offer := webrtc.SessionDescription{}
		decode(base, &offer)
		if err := peerConnection.SetRemoteDescription(offer); err != nil {
			return err
		}

		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			return err
		}

		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
		if err := peerConnection.SetLocalDescription(answer); err != nil {
			return err
		}

		<-gatherComplete
		return c.SendString(encode(peerConnection.LocalDescription()))

	})
	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendString("Hello, World!")
	})

	app.Get("/getFiles", func(c *fiber.Ctx) error {

		dir := "./files" // Adjust this path as needed

		// Open the directory
		f, err := os.Open(dir)
		if err != nil {
			log.Fatalf("Failed to open directory: %v", err)
		}
		defer f.Close()

		// Read the directory contents
		entries, err := f.Readdirnames(-1) // -1 means to read all entries
		if err != nil {
			log.Fatalf("Failed to read directory entries: %v", err)
		}
		var uuids []string
		// Filter out only directories
		for _, entry := range entries {
			fullPath := filepath.Join(dir, entry)
			info, err := os.Stat(fullPath)
			if err != nil {
				log.Printf("Failed to stat file %s: %v", fullPath, err)
				continue
			}
			if info.IsDir() {
				// Check if the folder name looks like a UUID (e.g., 8-4-4-4-12 hexadecimal characters)
				if isUUID(entry) {
					uuids = append(uuids, entry)
				}
			}
		}
		if len(uuids) == 0 {
			return c.SendString("No UUID folders found.")
		}

		// Join UUIDs with newline and send as response
		return c.JSON(fiber.Map{
			"uuids": uuids,
		})
	})
	app.Post("/", func(c *fiber.Ctx) error {
		var body map[string]interface{}
		if err := c.BodyParser(&body); err != nil {
			return err
		}
		param, ok := body["param"].(string)
		if !ok {
			return c.SendString("Parameter 'param' not found or not a string")
		}

		m := &webrtc.MediaEngine{}

		// Setup the codecs you want to use.
		// We'll use a VP8 and Opus but you can also define your own
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
			PayloadType:        96,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			panic(err)
		}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
			PayloadType:        111,
		}, webrtc.RTPCodecTypeAudio); err != nil {
			panic(err)
		}

		// Create a InterceptorRegistry. This is the user configurable RTP/RTCP Pipeline.
		// This provides NACKs, RTCP Reports and other features. If you use `webrtc.NewPeerConnection`
		// this is enabled by default. If you are manually managing You MUST create a InterceptorRegistry
		// for each PeerConnection.
		i := &interceptor.Registry{}

		// Register a intervalpli factory
		// This interceptor sends a PLI every 3 seconds. A PLI causes a video keyframe to be generated by the sender.
		// This makes our video seekable and more error resilent, but at a cost of lower picture quality and higher bitrates
		// A real world application should process incoming RTCP packets from viewers and forward them to senders
		intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
		if err != nil {
			panic(err)
		}
		i.Add(intervalPliFactory)

		// Use the default set of Interceptors
		if err = webrtc.RegisterDefaultInterceptors(m, i); err != nil {
			panic(err)
		}

		// Create the API object with the MediaEngine
		api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

		// Prepare the configuration
		config := webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					URLs: []string{"stun:stun.l.google.com:19302"},
				},
			},
		}

		// Create a new RTCPeerConnection
		peerConnection, err := api.NewPeerConnection(config)
		if err != nil {
			panic(err)
		}

		// Allow us to receive 1 audio track, and 1 video track
		if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
			panic(err)
		} else if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
			panic(err)
		}
		// id := uuid.New()
		// oggfs := afero.NewOsFs()

		// destPathIvf := "files/" + id.String() + "/output.ivf"
		// destpathOgg := "files/" + id.String() + "/output.opus"

		// // Move the file
		// errogg := oggfs.Mkdir("files/"+id.String(), 48000)
		// if errogg != nil {
		// 	fmt.Println("Error creating directory:", errogg)
		// } else {
		// 	fmt.Println("Directory created successfully!")
		// }

		// destPathIvf := "files/" + id.String() + "/output.ivf"

		oggFile, err := oggwriter.New("output.opus", 48000, 2)
		if err != nil {
			panic(err)
		}
		ivfFile, err := ivfwriter.New("output.ivf")
		if err != nil {
			panic(err)
		}

		// Set a handler for when a new remote track starts, this handler saves buffers to disk as
		// an ivf file, since we could have multiple video tracks we provide a counter.
		// In your application this is where you would handle/process video
		peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) { //nolint: revive
			codec := track.Codec()
			if strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
				fmt.Println("Got Opus track, saving to disk as output.opus (48 kHz, 2 channels)")
				saveToDisk(oggFile, track)
			} else if strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP8) {
				fmt.Println("Got VP8 track, saving to disk as output.ivf")
				saveToDisk(ivfFile, track)
			}
		})

		// Set the handler for ICE connection state
		// This will notify you when the peer has connected/disconnected
		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			fmt.Printf("Connection State has changed %s \n", connectionState.String())

			if connectionState == webrtc.ICEConnectionStateConnected {
				fmt.Println("Ctrl+C the remote client to stop the demo")
			} else if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateClosed || connectionState == webrtc.ICEConnectionStateDisconnected {
				if closeErr := oggFile.Close(); closeErr != nil {
					panic(closeErr)
				}

				if closeErr := ivfFile.Close(); closeErr != nil {
					panic(closeErr)
				}
				// id := uuid.New()
				// fs := afero.NewOsFs()
				// dirPath := "files/" + id.String() // Replace with your actual directory ID or name

				// err := fs.MkdirAll(dirPath, 0755)
				// if err != nil {
				// 	fmt.Println("Error creating directory:", err)
				// } else {
				// 	fmt.Println("Directory created successfully!")
				// }
				// ivffs := afero.NewOsFs()

				// destPathIvf := "files/" + id.String() + "/output.ivf"

				// // Move the file
				// errivf := ivffs.Rename("output.ivf", destPathIvf)
				// if errivf != nil {
				// 	fmt.Println("Error moving file:", errivf)
				// } else {
				// 	fmt.Println("File moved successfully!")
				// }

				// oggfs := afero.NewOsFs()

				// destPathOgg := "files/" + id.String() + "/output.ogg"

				// errogg := oggfs.Rename("output.ogg", destPathOgg)
				// if errogg != nil {
				// 	fmt.Println("Error moving file:", errogg)
				// } else {
				// 	fmt.Println("File moved successfully!")
				// }

				// if err != nil {
				// 	fmt.Println(err)
				// } else {
				// 	fmt.Println("Directory created successfully!")
				// }

				fmt.Println("Done writing media files")

				// Gracefully shutdown the peer connection
				if closeErr := peerConnection.Close(); closeErr != nil {
					panic(closeErr)
				}

				// os.Exit(0)

			}
		})

		// Wait for the offer to be pasted
		offer := webrtc.SessionDescription{}
		decode(param, &offer)

		// Set the remote SessionDescription
		err = peerConnection.SetRemoteDescription(offer)
		if err != nil {
			panic(err)
		}

		// Create answer
		answer, err := peerConnection.CreateAnswer(nil)
		if err != nil {
			panic(err)
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

		// Sets the LocalDescription, and starts our UDP listeners
		err = peerConnection.SetLocalDescription(answer)
		if err != nil {
			panic(err)
		}

		// Block until ICE Gathering is complete, disabling trickle ICE
		// we do this because we only can exchange one signaling message
		// in a production application you should exchange ICE Candidates via OnICECandidate
		<-gatherComplete

		// Output the answer in base64 so we can paste it in browser
		return c.SendString(encode(peerConnection.LocalDescription()))

		// // Block forever
		// select {}

		// return c.SendString("POST request " + param)
	})

	log.Fatal(app.Listen(":4000"))
}

func readUntilNewline(param any) (in string) {
	var err error

	r := bufio.NewReader(os.Stdin)
	for {
		in, err = r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			panic(err)
		}

		if in = strings.TrimSpace(in); len(in) > 0 {
			break
		}
	}

	fmt.Println("")
	return
}

// JSON encode + base64 a SessionDescription
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}
