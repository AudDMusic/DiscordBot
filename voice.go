package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/Mihonarium/dgvoice"
	"github.com/Mihonarium/discordgo"
	"github.com/cryptix/wav"
	"io/ioutil"
	"os"
	"time"
)

func CreateAndStartBuffer(s *discordgo.Session, guildID, channelID string) error {
	mu.Lock()
	existedBuf, alreadySet := serverBuffers[guildID+"-"+channelID]
	if alreadySet {
		existedBuf.Stop()
	}
	buf, err := startBuffer(s, guildID, channelID)
	if err != nil {
		mu.Unlock()
		return err
	}
	serverBuffers[guildID+"-"+channelID] = buf
	mu.Unlock()
	return nil
}
func startBuffer(s *discordgo.Session, guildID, channelID string) (serverBuffer, error) {
	vc, err := s.ChannelVoiceJoin(guildID, channelID, true, false, &h)
	if err != nil {
		return serverBuffer{}, err
	}
	recv := make(chan *discordgo.Packet, 2)
	go dgvoice.ReceivePCM(vc, recv)

	onClose := func() {
		capture(vc.Disconnect())
	}

	audioBuf, started, stop := listenBuffer(recv, time.Second*12, onClose)
	return serverBuffer{audioBuf, started, stop, ""}, nil
}
func (c *BotConfig) recordSound(s *discordgo.Session, guildID, channelID string, User string) ([]byte, error) {
	vc, err := s.ChannelVoiceJoin(guildID, channelID, true, false, &h)
	if err != nil {
		return nil, err
	}
	defer func() {
		capture(vc.Disconnect())
	}()
	recv := make(chan *discordgo.Packet, 2)
	go dgvoice.ReceivePCM(vc, recv)
	out, err := os.Create("output.pcm")
	if err != nil {
		return nil, err
	}
	defer captureFunc(out.Close)
	audioBuf, err := getWavAudio(recv, false, User)
	if err != nil {
		return nil, err
	}
	return audioBuf, nil
}

func getWavAudio(in chan *discordgo.Packet, readAll bool, User string) ([]byte, error) {
	file := wav.File{
		SampleRate:      48000,
		SignificantBits: 16,
		Channels:        1,
	}
	out, err := ioutil.TempFile("", "*.wav")
	if err != nil {
		return nil, err
	}
	writer, err := file.NewWriter(out)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	count := 0
	PCMStreams := make(map[string][]int16)
	for f := range in {
		if bytes.Equal(f.Type, []byte("stream-stop")) {
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
		//PCMStreams[u] = append(PCMStreams[u], f.PCM...)
		if !readAll {
			if start.Add(time.Second * 12).Before(time.Now()) {
				break
			}
		}
	}
	resultPCM := make([]int16, len(PCMStreams[User]))
	for j := range PCMStreams[User] {
		resultPCM[j] += PCMStreams[User][j]
	}
	for i := 0; i < len(resultPCM); i++ {
		buf := new(bytes.Buffer)
		err := binary.Write(buf, binary.LittleEndian, resultPCM[i])
		if err != nil {
			return nil, err
		}
		bytes_ := buf.Bytes()
		var byteSlice []byte
		byteSlice = append(byteSlice, bytes_[0])
		byteSlice = append(byteSlice, bytes_[1])
		err = writer.WriteSample(byteSlice)
		if err != nil {
			return nil, err
		}
	}
	capture(writer.Close())
	buf, err := ioutil.ReadFile(out.Name())
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func listenBuffer(in chan *discordgo.Packet, size time.Duration, onClose func()) (chan *discordgo.Packet, chan struct{}, chan struct{}) {
	started := make(chan struct{}, 2)
	stop := make(chan struct{}, 2)
	out := make(chan *discordgo.Packet, 50000)
	go func() {
		defer onClose()
		buffered := false
		var ticker *time.Ticker
		isStarted := false
		for f := range in {
			select {
			case <-started:
				isStarted = true
			case <-stop:
				close(out)
				return
			default:
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
					out <- f
				}
			}
			if buffered && !isStarted {
				<-out
				out <- f
			}
			if isStarted && buffered {
				out <- &discordgo.Packet{
					Type: []byte("stream-stop"),
				}
				isStarted = false
				ticker = nil
				buffered = false
			}
		}
	}()
	return out, started, stop
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
