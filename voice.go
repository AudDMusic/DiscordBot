package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/Mihonarium/dgvoice"
	"github.com/Mihonarium/discordgo"
	"github.com/cryptix/wav"
	"github.com/orcaman/writerseeker"
	"io"
	"sync"
	"time"
)

func CreateAndStartBuffer(s *discordgo.Session, guildID, channelID, initiatedByUserID string) error {
	mu.Lock()
	existedBuf, alreadySet := serverBuffers[guildID+"-"+channelID]
	if alreadySet {
		existedBuf.Stop()
		delete(serverBuffers, guildID+"-"+channelID)
	}
	buf, err := startBuffer(s, guildID, channelID, initiatedByUserID)
	if err != nil {
		mu.Unlock()
		return err
	}
	serverBuffers[guildID+"-"+channelID] = buf
	mu.Unlock()
	return nil
}
func StopBuffer(guildID, channelID string) {
	mu.Lock()
	existedBuf, alreadySet := serverBuffers[guildID+"-"+channelID]
	if alreadySet {
		existedBuf.Stop()
	}
	delete(serverBuffers, guildID+"-"+channelID)
	mu.Unlock()
}
func startBuffer(s *discordgo.Session, guildID, channelID, initiatedByUserID string) (serverBuffer, error) {
	vc, err := s.ChannelVoiceJoin(guildID, channelID, true, false, &h)
	if err != nil {
		return serverBuffer{}, err
	}
	recv := make(chan *discordgo.Packet, 2)
	go dgvoice.ReceivePCM(vc, recv)

	onClose := func() {
		capture(vc.Disconnect())
	}

	audioBuf, started, stop := listenBuffer(recv, time.Second*time.Duration(RecordSeconds), onClose)
	return serverBuffer{buf: audioBuf, start: started, stop: stop, InitiatedByUser: initiatedByUserID}, nil
}

const RecordSeconds = 12

func exitStreamsOnMute(alreadyCancelled *bool, cancelMu *sync.Mutex, recv chan *discordgo.Packet) {
	sleepBeforeCheckingForMute := 5
	time.Sleep(time.Second * time.Duration(sleepBeforeCheckingForMute))
	cancelMu.Lock()
	if *alreadyCancelled {
		return
	}
	select {
	case recv <- &discordgo.Packet{
		Type: []byte("mute-check"),
	}:
	default:
	}
	cancelMu.Unlock()
	time.Sleep(time.Second * time.Duration(RecordSeconds-sleepBeforeCheckingForMute+2))
	cancelMu.Lock()
	if *alreadyCancelled {
		return
	}
	select {
	case recv <- &discordgo.Packet{
		Type: []byte("check-exit"),
	}:
	default:
	}
	cancelMu.Unlock()

}

func (c *BotConfig) recordSound(s *discordgo.Session, guildID, channelID, userToListenToID string) ([]byte, error) {
	vc, err := s.ChannelVoiceJoin(guildID, channelID, true, false, &h)
	if err != nil {
		return nil, err
	}
	cancelled := false
	cancelMu := &sync.Mutex{}
	defer func() {
		cancelMu.Lock()
		cancelled = true
		cancelMu.Unlock()
		capture(vc.Disconnect())
	}()
	recv := make(chan *discordgo.Packet, 2)
	go dgvoice.ReceivePCM(vc, recv)
	go exitStreamsOnMute(&cancelled, cancelMu, recv)
	// out, err := os.Create("output.pcm")
	if err != nil {
		return nil, err
	}
	// defer captureFunc(out.Close)
	audioBuf, err := getWavAudio(recv, false, userToListenToID)
	if err != nil {
		return nil, err
	}
	return audioBuf, nil
}

func getWavAudio(in chan *discordgo.Packet, readAll bool, userToListenToID string) ([]byte, error) {
	file := wav.File{
		SampleRate:      48000,
		SignificantBits: 16,
		Channels:        1,
	}
	out := &writerseeker.WriterSeeker{}
	writer, err := file.NewWriter(out)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	count := 0
	PCMStreams := make(map[string][]int16)
	for f := range in {
		if bytes.Equal(f.Type, []byte("mute-check")) {
			// Exit if the voice channel was mute
			if count == 0 {
				break
			}
			continue
		}
		if bytes.Equal(f.Type, []byte("stream-stop")) {
			break
		}
		if bytes.Equal(f.Type, []byte("check-exit")) {
			// So we can exit even if the voice channel has no sound after RecordSeconds seconds
			break
		}
		count++
		u := checkSSRC(f.SSRC)
		if u == "" {
			fmt.Println("Can't get a user for SSRC", f.SSRC)
			continue
		}
		int16Slice := convertPCMToMono(f.PCM)
		if PCMStreams[u] == nil {
			PCMStreams[u] = make([]int16, 0)
		}
		PCMStreams[u] = append(PCMStreams[u], int16Slice...)
		// PCMStreams[u] = append(PCMStreams[u], f.PCM...)
		if !readAll {
			if start.Add(time.Second * time.Duration(RecordSeconds)).Before(time.Now()) {
				break
			}
		}
	}
	if count == 0 {
		return nil, nil
	}
	resultPCM := make([]int16, len(PCMStreams[userToListenToID]))
	for j := range PCMStreams[userToListenToID] {
		resultPCM[j] += PCMStreams[userToListenToID][j]
	}
	for i := 0; i < len(resultPCM); i++ {
		buf := new(bytes.Buffer)
		err := binary.Write(buf, binary.LittleEndian, resultPCM[i])
		if err != nil {
			return nil, err
		}
		bytes_ := buf.Bytes()
		var byteSlice []byte
		byteSlice = append(byteSlice, bytes_[0], bytes_[1])
		err = writer.WriteSample(byteSlice)
		if err != nil {
			return nil, err
		}
	}
	capture(writer.Close())
	bytesBuf := &bytes.Buffer{}
	_, err = io.Copy(bytesBuf, out.Reader())
	if err != nil {
		return nil, err
	}
	return bytesBuf.Bytes(), nil
}

func listenBuffer(in chan *discordgo.Packet, size time.Duration, onClose func()) (audioBuffer chan *discordgo.Packet, started, stop chan struct{}) {
	started = make(chan struct{}, 2)
	stop = make(chan struct{}, 4)
	audioBuffer = make(chan *discordgo.Packet, 50000)
	cancelMu := &sync.Mutex{}
	cancelled := false
	go func() {
		// Added this so if there's no sound, the bot leaves the VC immediately without waiting for a packet
		<-stop
		in <- &discordgo.Packet{
			Type: []byte("stream-stop"),
		}
		// If there are a lot of packets in the queue,
		// this guarantees that the bot will leave after it's done with the current packet
		stop <- struct{}{}
	}()
	go func() {
		defer onClose()
		buffered := false
		var ticker *time.Ticker
		isStarted := false
		for f := range in {
			select {
			case <-stop:
				cancelMu.Lock()
				cancelled = true
				cancelMu.Unlock()
				close(audioBuffer)
				stop <- struct{}{}
				return
			case <-started:
				isStarted = true
				go exitStreamsOnMute(&cancelled, cancelMu, audioBuffer)
			default:
			}
			if bytes.Equal(f.Type, []byte("stream-stop")) {
				cancelMu.Lock()
				cancelled = true
				cancelMu.Unlock()
				close(audioBuffer)
				return
			}
			if ticker == nil {
				ticker = time.NewTicker(size)
			}
			if !buffered {
				select {
				case <-ticker.C:
					buffered = true
					ticker.Stop()
				default:
					audioBuffer <- f
				}
			}
			if buffered && !isStarted {
				<-audioBuffer
				audioBuffer <- f
			}
			if isStarted && buffered {
				audioBuffer <- &discordgo.Packet{
					Type: []byte("stream-stop"),
				}
				isStarted = false
				ticker = nil
				buffered = false
			}
		}
	}()
	return audioBuffer, started, stop
}

func convertPCMToMono(pcm []int16) []int16 {
	var monoPCM []int16
	for i := 0; i < len(pcm); i += 2 {
		sample1 := pcm[i] //+ pcm[i+1]
		monoPCM = append(monoPCM, sample1)
	}
	return monoPCM
}
func checkSSRC(ssrc uint32) string {
	mu.Lock()
	defer mu.Unlock()
	for u, s := range usersSSRCs {
		if ssrc == uint32(s) {
			return u
		}
	}
	return ""
}

var usersSSRCs = map[string]int{}

var h = discordgo.VoiceSpeakingUpdateHandler(func(vc *discordgo.VoiceConnection, vs *discordgo.VoiceSpeakingUpdate) {
	if vs.Speaking {
		mu.Lock()
		usersSSRCs[vs.UserID] = vs.SSRC
		mu.Unlock()
	} else {
		mu.Lock()
		delete(usersSSRCs, vs.UserID)
		mu.Unlock()
	}
})
