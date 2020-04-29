# discord-bot
A music recognition bot for Discord. Uses the [Music Recognition API](https://audd.io/).

## How to run it:
- Get a token from [AudD Telegram bot](https://t.me/auddbot?start=api) and copy it to the AudDToken variable


- Create an application here: https://discordapp.com/developers/applications
- Copy the secret to the DiscordToken variable and get the Client ID
- Create a bot
- Open `https://discordapp.com/api/oauth2/authorize?client_id=<INSERT CLIENT ID HERE>&permissions=1049088&scope=bot` and add the bot to a server
- Run the bot

## How to use it
- To recognize a song from a voice channel, type !song or !recognize
- It's better to also mention users who are playing the song (like !song @MusicGuy)
- If you want the bot to listen to a channel so it can immediately recognize the song from the last 15 second of audio, type !listen.
