package main

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"layeh.com/gopus"
)

const (
	RecordSeconds   = 12
	voiceSampleRate = 48000
	maxRingSamples  = RecordSeconds * voiceSampleRate
	// maxOpusFrame is the longest frame Opus can carry (120 ms at 48 kHz).
	maxOpusFrame = 5760
	// maxStreamsPerSession bounds the memory one voice channel can take (about 1.2 MB per speaker).
	maxStreamsPerSession = 16
	// staleAudioAfter is how old the last received audio can be to still count as "playing now".
	staleAudioAfter = 15 * time.Second
	// silenceCheckAfter is when a recording is given up on if nobody has made a sound.
	silenceCheckAfter = 5 * time.Second
)

var (
	errVoiceBusy     = errors.New("already listening to another voice channel on this server")
	errVoiceCapacity = errors.New("connected to too many voice channels")
)

// pcmStream keeps the last RecordSeconds of mono audio from one speaker.
type pcmStream struct {
	decoder     *gopus.Decoder
	samples     []int16
	lastPacket  time.Time
	nextRTP     uint32 // the RTP timestamp expected in the next packet
	initialized bool
}

func (st *pcmStream) append(samples []int16) {
	st.samples = append(st.samples, samples...)
	if len(st.samples) > 2*maxRingSamples {
		st.samples = append([]int16(nil), st.samples[len(st.samples)-maxRingSamples:]...)
	}
}

// write adds a decoded mono frame, filling the gaps Discord leaves when a speaker is silent.
func (st *pcmStream) write(rtpTimestamp uint32, mono []int16, now time.Time) {
	if st.initialized {
		gap := int32(rtpTimestamp - st.nextRTP)
		switch {
		case gap < 0 && gap > -maxRingSamples:
			return // a late or duplicated packet
		case gap < 0 || gap > maxRingSamples:
			st.samples = st.samples[:0] // the stream was restarted or was silent for a long time
		case gap > 0:
			st.append(make([]int16, gap))
		}
	}
	st.append(mono)
	st.initialized = true
	st.nextRTP = rtpTimestamp + uint32(len(mono))
	st.lastPacket = now
}

// snapshot returns a copy of the buffered audio, or nil if it's too old.
func (st *pcmStream) snapshot(now time.Time) []int16 {
	if now.Sub(st.lastPacket) > staleAudioAfter {
		return nil
	}
	samples := st.samples
	if len(samples) > maxRingSamples {
		samples = samples[len(samples)-maxRingSamples:]
	}
	return append([]int16(nil), samples...)
}

func stereoToMono(pcm []int16) []int16 {
	mono := make([]int16, len(pcm)/2)
	for i := range mono {
		mono[i] = int16((int32(pcm[2*i]) + int32(pcm[2*i+1])) / 2)
	}
	return mono
}

// encodeWAV wraps 16-bit mono PCM into a WAV file.
func encodeWAV(samples []int16, sampleRate int) []byte {
	dataSize := len(samples) * 2
	out := make([]byte, 44+dataSize)
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+dataSize))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)                   // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:], 1)                    // PCM
	binary.LittleEndian.PutUint16(out[22:], 1)                    // channels
	binary.LittleEndian.PutUint32(out[24:], uint32(sampleRate))   // sample rate
	binary.LittleEndian.PutUint32(out[28:], uint32(sampleRate*2)) // byte rate
	binary.LittleEndian.PutUint16(out[32:], 2)                    // block align
	binary.LittleEndian.PutUint16(out[34:], 16)                   // bits per sample
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(dataSize))
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[44+2*i:], uint16(sample))
	}
	return out
}

// voiceSession is one connection to a voice channel.
type voiceSession struct {
	guildID, channelID string
	invitedBy          string // set for the sessions started with /listen
	persistent         bool
	vc                 *discordgo.VoiceConnection
	done               chan struct{}
	closeOnce          sync.Once

	mu       sync.Mutex
	ssrcUser map[uint32]string
	streams  map[uint32]*pcmStream
	packets  int
	lastUsed time.Time
}

func (vs *voiceSession) onSpeaking(_ *discordgo.VoiceConnection, update *discordgo.VoiceSpeakingUpdate) {
	if update == nil || update.UserID == "" {
		return
	}
	vs.mu.Lock()
	vs.ssrcUser[uint32(update.SSRC)] = update.UserID
	vs.mu.Unlock()
}

func (vs *voiceSession) run(recv <-chan *discordgo.Packet) {
	defer recoverPanic("voice receiver")
	for {
		select {
		case <-vs.done:
			return
		case p, ok := <-recv:
			if !ok {
				return
			}
			if p != nil && len(p.Opus) > 0 {
				vs.handlePacket(p)
			}
		}
	}
}

func (vs *voiceSession) handlePacket(p *discordgo.Packet) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	now := time.Now()
	st := vs.streams[p.SSRC]
	if st == nil {
		if len(vs.streams) >= maxStreamsPerSession {
			var oldest uint32
			var oldestAt time.Time
			for ssrc, other := range vs.streams {
				if oldestAt.IsZero() || other.lastPacket.Before(oldestAt) {
					oldest, oldestAt = ssrc, other.lastPacket
				}
			}
			delete(vs.streams, oldest)
		}
		decoder, err := gopus.NewDecoder(voiceSampleRate, 2)
		if err != nil {
			return
		}
		st = &pcmStream{decoder: decoder}
		vs.streams[p.SSRC] = st
	}
	pcm, err := st.decoder.Decode(p.Opus, maxOpusFrame, false)
	if err != nil || len(pcm) < 2 {
		return
	}
	vs.packets++
	st.write(p.Timestamp, stereoToMono(pcm), now)
}

// audioOf returns what's buffered from the user.
func (vs *voiceSession) audioOf(userID string) (samples []int16, packets int) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	now := time.Now()
	var newest time.Time
	for ssrc, st := range vs.streams {
		if vs.ssrcUser[ssrc] == userID && st.lastPacket.After(newest) {
			if snapshot := st.snapshot(now); snapshot != nil {
				samples, newest = snapshot, st.lastPacket
			}
		}
	}
	if samples == nil && len(vs.streams) == 1 {
		// Only one participant is making any sound and Discord hasn't told us who that is.
		for ssrc, st := range vs.streams {
			if _, known := vs.ssrcUser[ssrc]; !known {
				samples = st.snapshot(now)
			}
		}
	}
	return samples, vs.packets
}

// record waits until there are RecordSeconds of audio from the user, but no longer than RecordSeconds.
func (vs *voiceSession) record(ctx context.Context, userID string) []int16 {
	vs.mu.Lock()
	vs.lastUsed = time.Now()
	vs.mu.Unlock()
	start := time.Now()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		samples, packets := vs.audioOf(userID)
		elapsed := time.Since(start)
		switch {
		case len(samples) >= maxRingSamples-voiceSampleRate/2, // (almost) full
			elapsed >= RecordSeconds*time.Second+500*time.Millisecond,
			packets == 0 && elapsed >= silenceCheckAfter: // the voice channel is silent
			return samples
		}
		select {
		case <-ticker.C:
		case <-vs.done:
			return samples
		case <-ctx.Done():
			return samples
		}
	}
}

// info returns who started the session with /listen (if anyone) and whether it stays after a recognition.
func (vs *voiceSession) info() (invitedBy string, persistent bool) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return vs.invitedBy, vs.persistent
}

func (vs *voiceSession) close() {
	vs.closeOnce.Do(func() {
		close(vs.done)
		go func() {
			defer recoverPanic("voice disconnect")
			if err := vs.vc.Disconnect(); err != nil {
				log.Println("error leaving a voice channel:", err)
			}
		}()
	})
}

type lastListened struct {
	userID string
	at     time.Time
}

// voiceManager owns the voice sessions. Discord allows one voice connection per server.
type voiceManager struct {
	maxSessions int
	idleTimeout time.Duration

	mu           sync.Mutex
	sessions     map[string]*voiceSession // by guild ID
	joining      map[string]bool
	lastListened map[string]lastListened // by guild ID + channel ID
}

func newVoiceManager(maxSessions int, idleTimeout time.Duration) *voiceManager {
	return &voiceManager{
		maxSessions: maxSessions, idleTimeout: idleTimeout,
		sessions: map[string]*voiceSession{}, joining: map[string]bool{}, lastListened: map[string]lastListened{},
	}
}

func (vm *voiceManager) get(guildID string) *voiceSession {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.sessions[guildID]
}

// join connects to the voice channel. For an existing session in the same
// channel, it returns that session (and makes it persistent if asked to).
func (vm *voiceManager) join(s *discordgo.Session, guildID, channelID, invitedBy string, persistent bool) (*voiceSession, bool, error) {
	vm.mu.Lock()
	if existing := vm.sessions[guildID]; existing != nil {
		defer vm.mu.Unlock()
		if existing.channelID != channelID {
			return nil, false, errVoiceBusy
		}
		if persistent {
			existing.mu.Lock()
			existing.persistent, existing.invitedBy = true, invitedBy
			existing.lastUsed = time.Now()
			existing.mu.Unlock()
		}
		return existing, false, nil
	}
	if vm.joining[guildID] {
		vm.mu.Unlock()
		return nil, false, errVoiceBusy
	}
	if vm.maxSessions >= 0 && len(vm.sessions)+len(vm.joining) >= vm.maxSessions {
		vm.mu.Unlock()
		return nil, false, errVoiceCapacity
	}
	vm.joining[guildID] = true
	vm.mu.Unlock()
	defer func() {
		vm.mu.Lock()
		delete(vm.joining, guildID)
		vm.mu.Unlock()
	}()

	vc, err := s.ChannelVoiceJoin(guildID, channelID, true, false)
	if err != nil {
		if vc != nil {
			_ = vc.Disconnect()
		}
		return nil, false, err
	}
	vs := &voiceSession{
		guildID: guildID, channelID: channelID, invitedBy: invitedBy, persistent: persistent, vc: vc,
		done: make(chan struct{}), ssrcUser: map[uint32]string{}, streams: map[uint32]*pcmStream{},
		lastUsed: time.Now(),
	}
	vc.AddHandler(vs.onSpeaking)
	var recv chan *discordgo.Packet
	for i := 0; i < 20 && recv == nil; i++ { // the channel is created when the UDP connection is up
		vc.RLock()
		recv = vc.OpusRecv
		vc.RUnlock()
		if recv == nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if recv == nil {
		_ = vc.Disconnect()
		return nil, false, errors.New("the voice connection didn't start receiving audio")
	}
	go vs.run(recv)
	vm.mu.Lock()
	vm.sessions[guildID] = vs
	vm.mu.Unlock()
	return vs, true, nil
}

// leave ends the session in the server if it is the given one (or any, when vs is nil).
func (vm *voiceManager) leave(guildID string, vs *voiceSession) {
	vm.mu.Lock()
	current := vm.sessions[guildID]
	if current == nil || vs != nil && current != vs {
		vm.mu.Unlock()
		return
	}
	delete(vm.sessions, guildID)
	vm.mu.Unlock()
	current.close()
}

func (vm *voiceManager) leaveAll() {
	vm.mu.Lock()
	sessions := vm.sessions
	vm.sessions = map[string]*voiceSession{}
	vm.mu.Unlock()
	for _, vs := range sessions {
		vs.close()
	}
}

func (vm *voiceManager) rememberListened(guildID, channelID, userID string) {
	vm.mu.Lock()
	vm.lastListened[guildID+"-"+channelID] = lastListened{userID: userID, at: time.Now()}
	vm.mu.Unlock()
}

func (vm *voiceManager) lastListenedTo(guildID, channelID string) string {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.lastListened[guildID+"-"+channelID].userID
}

// cleanup leaves the voice channels nobody has used for a while and forgets old data.
func (vm *voiceManager) cleanup() {
	now := time.Now()
	var idle []*voiceSession
	vm.mu.Lock()
	for _, vs := range vm.sessions {
		vs.mu.Lock()
		if vs.persistent && vm.idleTimeout > 0 && now.Sub(vs.lastUsed) > vm.idleTimeout {
			idle = append(idle, vs)
		}
		vs.mu.Unlock()
	}
	for key, last := range vm.lastListened {
		if now.Sub(last.at) > 24*time.Hour {
			delete(vm.lastListened, key)
		}
	}
	vm.mu.Unlock()
	for _, vs := range idle {
		vm.leave(vs.guildID, vs)
	}
}

// userVoiceChannel returns the voice channel the user is in ("" if none).
func userVoiceChannel(s *discordgo.Session, guildID, userID string) string {
	if guildID == "" || userID == "" {
		return ""
	}
	state, err := s.State.VoiceState(guildID, userID)
	if err != nil || state == nil {
		return ""
	}
	return state.ChannelID
}

// humansInChannel counts the members of the voice channel that aren't bots.
func humansInChannel(s *discordgo.Session, guildID, channelID string) int {
	g, err := s.State.Guild(guildID)
	if err != nil {
		return 0
	}
	s.State.RLock()
	defer s.State.RUnlock()
	count := 0
	for _, state := range g.VoiceStates {
		if state == nil || state.ChannelID != channelID {
			continue
		}
		if state.Member != nil && state.Member.User != nil && state.Member.User.Bot {
			continue
		}
		if s.State.User != nil && state.UserID == s.State.User.ID {
			continue
		}
		count++
	}
	return count
}

// onVoiceStateUpdate makes the bot leave when it's left alone, moved or disconnected by someone.
func (b *Bot) onVoiceStateUpdate(s *discordgo.Session, update *discordgo.VoiceStateUpdate) {
	defer recoverPanic("voiceStateUpdate")
	if update.VoiceState == nil || update.GuildID == "" {
		return
	}
	vs := b.voice.get(update.GuildID)
	if vs == nil {
		return
	}
	if s.State.User != nil && update.UserID == s.State.User.ID {
		if update.ChannelID != vs.channelID {
			b.voice.leave(update.GuildID, vs)
		}
		return
	}
	if humansInChannel(s, vs.guildID, vs.channelID) == 0 {
		b.voice.leave(update.GuildID, vs)
	}
}

const (
	replyVoiceUnavailable = "Sorry, recognizing music from voice channels is currently unavailable. " +
		"You can still reply with **!song** to a message with an audio or video file or a link, " +
		"or right-click a message and pick Apps -> Recognize This Song."
	replyNotInVoice = "You need to be in a voice channel and mention a user " +
		"playing music there or you need to reply to an audio file or URL to identify music from"
	replyMentionSpeaker = "Please mention the user playing the music in a voice channel (like !song @musicbot) or " +
		"reply with !song to a message with an audio file or a link to the audio file and I'll identify the music"
	replyNoCapture = "Sorry, I couldn't capture any audio"
)

// SongVCCommand records the user in the requester's voice channel and identifies the music.
// replyInAnyCase tells whether the request was clearly meant for a voice channel.
func (b *Bot) SongVCCommand(ctx context.Context, s *discordgo.Session, userID, userToListenToID, guildID string,
	reference *discordgo.MessageReference, canCompress bool) (replyInAnyCase bool, message *discordgo.MessageSend) {
	channelID := userVoiceChannel(s, guildID, userID)
	if channelID == "" {
		return false, textMessage(replyNotInVoice, reference)
	}
	if userToListenToID == "" {
		userToListenToID = b.voice.lastListenedTo(guildID, channelID)
	}
	if userToListenToID == "" {
		return true, textMessage(replyMentionSpeaker, reference)
	}
	if userVoiceChannel(s, guildID, userToListenToID) != channelID {
		return true, textMessage("The user you mentioned isn't in your voice channel", reference)
	}
	if ok, reply := b.admit(userID, guildID); !ok {
		if reply == "" {
			return false, nil
		}
		return true, textMessage(reply, reference)
	}
	b.voice.rememberListened(guildID, channelID, userToListenToID)

	vs, _, err := b.voice.join(s, guildID, channelID, "", false)
	switch {
	case errors.Is(err, errVoiceBusy):
		return true, textMessage("Sorry, I'm already listening to another voice channel on this server", reference)
	case errors.Is(err, errVoiceCapacity):
		return true, textMessage("Sorry, I'm in too many voice channels right now. Please try again later.", reference)
	case err != nil:
		log.Println("can't join a voice channel:", err)
		return true, textMessage("Sorry, I couldn't join the voice channel", reference)
	}
	samples := vs.record(ctx, userToListenToID)
	if _, persistent := vs.info(); !persistent {
		b.voice.leave(guildID, vs) // nobody asked the bot to stay with /listen
	}
	if len(samples) < voiceSampleRate { // less than a second
		return true, textMessage(replyNoCapture, reference)
	}
	return true, b.recognizeFromAudio(ctx, encodeWAV(samples, voiceSampleRate), reference, canCompress)
}

// ListenCommand makes the bot join the user's voice channel and keep the last seconds of audio.
func (b *Bot) ListenCommand(s *discordgo.Session, guildID, userID string) string {
	channelID := userVoiceChannel(s, guildID, userID)
	if channelID == "" {
		return "Sorry, I can't find a voice channel you're in"
	}
	if !b.limiter.Allow("listen:"+userID, 6, time.Minute) {
		return replyUserLimited
	}
	_, _, err := b.voice.join(s, guildID, channelID, userID, true)
	switch {
	case errors.Is(err, errVoiceBusy):
		return "Sorry, I'm already listening to another voice channel on this server"
	case errors.Is(err, errVoiceCapacity):
		return "Sorry, I'm in too many voice channels right now. Please try again later."
	case err != nil:
		log.Println("can't join a voice channel:", err)
		return "Sorry, I couldn't join the voice channel"
	}
	return "Listening!\nType !song with a mention to recognize a song played by someone mentioned."
}

// StopListeningCommand makes the bot leave the voice channel if the user is allowed to ask for that.
func (b *Bot) StopListeningCommand(s *discordgo.Session, guildID, userID string) (left bool, response string) {
	vs := b.voice.get(guildID)
	if vs == nil {
		return false, "I'm not listening to a voice channel on this server"
	}
	invitedBy, _ := vs.info()
	if invitedBy == userID {
		// Allow kicking the bot by the user who invited it
		b.voice.leave(guildID, vs)
		return true, "Bye!"
	}
	if userVoiceChannel(s, guildID, userID) != vs.channelID {
		return false, "You can use this command if you invited me to listen to a voice channel or if you're on a voice channel " +
			"with me and the person who invited me has left"
	}
	if invitedBy == "" || userVoiceChannel(s, guildID, invitedBy) != vs.channelID {
		// Allow kicking the bot by any user on the voice channel if the user who invited it has left
		b.voice.leave(guildID, vs)
		return true, "Bye!"
	}
	return false, "Sorry, if the person who invited me to listen to the voice channel is still on the voice channel, only " +
		"they can use this command"
}
