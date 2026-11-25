# DiscordBot

A music recognition bot for Discord. Uses the [Music Recognition API](https://audd.io/).

[Try it on our server!](https://audd.cc/discord)

[![Discord bot](https://audd.tech/discord.jpg?)](https://www.youtube.com/watch?v=HcORAQzwTdM)

## How to run it
- Get a token from [AudD](https://dashboard.audd.io).
- Create an application here: https://discord.com/developers/applications and create a bot in it.
- Copy `config.example.json` to `config.json` and put the AudD token into `AudDToken` and the bot token into `DiscordToken`.
  `config.json` is in `.gitignore`, so the tokens don't end up in git. You can also leave them out of the file and set the
  `AUDD_API_TOKEN` and `DISCORD_TOKEN` environment variables (and `SENTRY_DSN`, `CALLBACK_SECRET`) instead.
- Build the binary (`go build -o discordBot .`; you need Go and a C compiler) or download
  [one compiled by GitHub](https://github.com/AudDMusic/DiscordBot/actions/workflows/go.yml), and run it: `./discordBot`
  (or `./discordBot -config /path/to/config.json`).
- Open `https://discord.com/api/oauth2/authorize?client_id=<INSERT CLIENT ID HERE>&permissions=277026819136&scope=bot%20applications.commands` and add the bot to a server.

**To identify music from the messages you reply to with "!song", the bot needs the privileged Message Content intent**
(Bot -> Privileged Gateway Intents in the Discord developer portal). The bot checks at startup whether Discord has
granted it. Without it, the bot still runs: the "Recognize This Song" context-menu command and the slash commands work,
and text triggers work in DMs and in the messages that mention the bot. Restart the bot after the intent is granted.

## How to use it
- To identify a song from an audio/video file or a link, reply to it with `!song` or right-click on the message and pick Apps -> Recognize This Song.
- Add a time to listen to a specific part: `!song 1:25`, `!song 1:25-2:10`, or `!song at the end`.
- `!help` or `/help` shows the help message.
- `/privacy` lets anyone opt out of the bot processing their messages and voice, opt back in, or see the [privacy policy](https://audd.io/privacy/discord-bot/). See [PRIVACY.md](PRIVACY.md).

### Voice channels
- To recognize music from a voice channel, send `!song @mention` or use the `/song-vc` slash command, mentioning the person who is playing the song (like `!song @MusicBot`).
- If you want the bot to stay in the channel so it can immediately recognize the song from the last 12 seconds of audio, type `!listen` or use `/listen`. `/disconnect` makes it leave. It also leaves on its own when everyone else has left or when it hasn't been used for `VoiceIdleMinutes`.

> [!IMPORTANT]
> The voice commands are off by default (`"VoiceEnabled": false`). Since March 2026, Discord only accepts voice
> connections that support its end-to-end encryption protocol (DAVE), and [discordgo](https://github.com/bwmarrin/discordgo),
> the library this bot uses, [doesn't support it yet](https://github.com/bwmarrin/discordgo/issues/1697). Until it does,
> the bot can't join voice channels: with `VoiceEnabled` set to `true`, the commands appear but fail to connect.

## Running it as a service
[`discordbot.service`](discordbot.service) is a systemd unit with a sandbox and memory limits. Adjust the paths and the
user in it, copy it to `/etc/systemd/system/` and run `systemctl enable --now discordbot`. The bot exits with status 78
when its config is broken or Discord rejects its token, and the unit doesn't restart it in that case.

## Limits and abuse prevention
Every recognition is an API request, so the bot limits how many of them one user or one server can cause. The defaults
are in `config.example.json`:

| Setting | Default | What it does |
|---|---|---|
| `RateLimits.UserPerMinute` / `UserPerDay` | 5 / 150 | Recognitions one user can start |
| `RateLimits.GuildPerMinute` / `GuildPerDay` | 20 / 2000 | Recognitions all the users of one server can start |
| `RateLimits.MaxConcurrent` | 8 | Recognitions running at the same time |
| `ResultCacheMinutes` | 10 | How long the result for the same link is reused instead of asking the API again |
| `MaxVoiceSessions` | 25 | Voice channels the bot can be in at once |
| `BlockedUserIDs`, `BlockedGuildIDs` | empty | Users and servers the bot ignores |
| `OptOutFile` | `optouts.json` | Where the IDs of the users who opted out with `/privacy` are kept. The bot needs to be able to write to its folder |
| `MaxTriggerTextLength` | 0 (off) | Ignore the messages longer than this |

A value of `0` means "use the default", and a negative value turns that limit off. A user who goes over a limit
gets an ⏳ reaction (at most once a minute) or, for the slash and context-menu commands, a reply only they can see.

The bot also never replies to other bots, never pings anyone in its replies (whatever is in the song's metadata),
and only passes plain `http(s)` links to public hosts on for recognition.

## How to use it with the streams

If you have a stream, with this bot you can automatically post all the songs to Discord.

![Discord bot](https://audd.tech/discord2.png)

### How to run it for streams
- Add a stream to the API with the [Music Recognition API for streams](https://docs.audd.io/streams/).
- Set `SecretCallbackToken` in *config.json* to a long random string (at least 16 characters; for example, the output of
  `openssl rand -hex 24`). The callback server doesn't start without it.
- Change `127.0.0.1:port` to `:port` in `CallbacksAddr`, or better, keep it on 127.0.0.1 behind a reverse proxy with HTTPS.
- Make a setCallbackUrl API request:
  * `https://api.audd.io/setCallbackUrl/?api_token=YOUR_AUDD_TOKEN&url=http://YOUR_SERVER_IP:4541/?secret=SECRET_CALLBACK_TOKEN%26chat_id=CHAT_ID`
  * CHAT_ID is the ID of the Discord channel where the bot will post the recognition results. Send `!here` in a channel to get its ID.
  * Add `%26publishAnnouncement=true` to also publish the messages in an announcement channel.

## Development
```
go vet ./...
go test -race ./...
```
