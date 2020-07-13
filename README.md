# discord-bot
A music recognition bot for Discord. Uses the [Music Recognition API](https://audd.io/).

![Discord bot](https://audd.tech/discord.jpg?)

## How to run it:
- Get a token from [AudD Telegram bot](https://t.me/auddbot?start=api) and copy it to the AudDToken variable
- Create an application here: https://discordapp.com/developers/applications
- Copy the secret to the DiscordToken variable and get the Client ID
- Create a bot
- Run the discord-bot (e.g. `go run main.go`)
- Open `https://discordapp.com/api/oauth2/authorize?client_id=<INSERT CLIENT ID HERE>&permissions=1049088&scope=bot` and add the bot to a server

## How to use it
- To recognize a song from a voice channel, type !song or !recognize
- It's better to also mention users who are playing the song (like !song @MusicBot)
- If you want the bot to listen to a channel so it can immediately recognize the song from the last 15 second of audio, type !listen.

## How to use it with the streams

If you have a stream, with this bot you can automatically post all the songs to Discord.

![Discord bot](https://audd.tech/discord2.png)

### How to run it for streams
- Add a stream to the API with the [Music Recognition API for streams](https://streams.audd.io/)
- uncomment lines 39 and 63
- make a setCallbackUrl request:
  * https://api.audd.io/setCallbackUrl/?api_token=YOUR_TOKEN&url=http://YOUR_SERVER_IP:4545/?secret=SECRET_CALLBACK_TOKEN%26chats=CHAT_LIST
  * CHAT_LIST is a string with JSON of radio_ids and comma-separated Discord text channel ids, like `{"1":"705141908...,719623447...,731869898...","2":"731869943..."}`
  * SECRET_CALLBACK_TOKEN is any string you want. Need it to ensure the callbacks are from a trusted source.
- set the value of the secretCallbackToken variable on line 39 to SECRET_CALLBACK_TOKEN from above 

Quik tip: the bot prints IDs of all the text channel it has access to when it restarts or is being added to a new server.
