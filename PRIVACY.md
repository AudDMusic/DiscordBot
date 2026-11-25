# Privacy

The privacy policy of the AudD Discord bot is at **https://audd.io/privacy/discord-bot/**.

In short:

- The bot does not store your messages. It checks the messages it can see for requests like `!song`, in memory, and
  discards everything else.
- When someone asks it to identify music, it sends the link to that audio or video file to the AudD Music Recognition API.
- When someone sends it a direct message with an invite to a voice channel, it asks Discord where the invite leads and,
  if that person is in the channel, joins it.
- Anyone can stop the bot from processing their messages and voice with `/privacy` -> opt-out. The bot then keeps only
  their user ID, in the file set by `OptOutFile` (`optouts.json` by default), to remember the choice.

If you run your own copy of the bot, you are responsible for its data: publish your own policy and change
`privacyPolicyURL` in `optout.go` to point to it.
