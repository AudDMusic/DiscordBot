# discord-bot
A music recognition bot for Discord. Uses the [Music Recognition API](https://audd.io/).

![Discord bot](https://audd.tech/discord.jpg?)[https://www.youtube.com/watch?v=HcORAQzwTdM]

## How to run it:
- Get a token from [AudD](https://dashboard.audd.io) and copy it to the AudDToken in *config.json*
- Create an application here: https://discordapp.com/developers/applications
- Copy the secret to the DiscordToken in config.json and the Client ID to DiscordAppID in *config.json*
- Create a bot
- Build and run the discordBot (e.g. `go build && ./discordBot`)
- Open `https://discordapp.com/api/oauth2/authorize?client_id=<INSERT CLIENT ID HERE>&permissions=277026819136&scope=bot%20applications.commands` and add the bot to a server

## How to use it
- To identify a song from an audio/video file or a link, reply to it with !song or or right-click on the message and pick App -> Recognize This Song
- To recognize music from a voice channel, send `!song @mention` or /vs-song slash command, mentioning the person who is playing the song (like !song @MusicBot)
- If you want the bot to listen to a channel so it can immediately recognize the song from the last 15 second of audio, type !listen or use the /listen slash command.

## How to use it with the streams

If you have a stream, with this bot you can automatically post all the songs to Discord.

![Discord bot](https://audd.tech/discord2.png)

### How to run it for streams
- Add a stream to the API with the [Music Recognition API for streams](https://docs.audd.io/streams/)
- change `127.0.0.1:port` to `:port` in *config.json* 
- make a setCallbackUrl API request:
  * https://api.audd.io/setCallbackUrl/?api_token=YOUR_AUDD_TOKEN&url=http://YOUR_SERVER_IP:4541/?secret=SECRET_CALLBACK_TOKEN%26chat=CHAT_ID
  * CHAT_ID is the Discord chat ID where the bot will post the recognition results.
  * SECRET_CALLBACK_TOKEN is any string you want. Need it to ensure the callbacks are from a trusted source. Add it to *config.json*.

The bot prints IDs of all the text channel it has access to when it restarts or is being added to a new server or on the !here command.
