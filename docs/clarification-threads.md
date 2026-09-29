# Clarification threads

When an agent in a Kandev task needs your input, it calls `ask_user_question_kandev`.
Kandev then holds a *pending question* (1 to 4 questions, some with options) and the agent waits.
With clarification threads on, the plugin posts those questions to Slack as a new thread.
Your reply in that thread answers them, and the agent resumes.

```
Kandev:  ❓ An agent needs your answer — Spec the login flow (PLAT-7)
         > Planning the spec
         1. Which database?
               a) Postgres
               b) SQLite — embedded
         2. Anything out of scope?
         Reply in this thread to answer, one numbered line per question (1. …, 2. …).
you:     1. b
         2. billing
Kandev:  Answered — the agent resumes.            (✅ on your reply)
```

This suits a workflow step where a strong model asks questions before it plans.

## How it works

- **Finding questions.** Kandev has no event for a new question.
  The plugin checks the pending list (`Interactions().ListPending`) every few seconds.
  Each question is posted once.
- **Choosing the channel.** The plugin reads the task to learn its workspace.
  It uses the workspace's mapped channel, or the default channel.
- **Remembering threads.** Kandev plugin state keeps a map from each pending id to its channel and thread.
  An entry is removed when its question is resolved.
- **Reading your reply.**
  - One question: the whole reply is the answer.
  - Several questions: number your lines (`1. …`, `2. …`). Lines below a number belong to it.
    If you number some questions but skip one, the plugin asks you to reply again.
    Kandev only accepts a full set of answers.
  - Several questions, no numbers: the whole reply answers every question.
  - An option letter (`b`), an option id, or an option's exact label picks that option.
    Anything else is sent as your own words.
- **Who can answer.** Only the Slack users in the allow-list.
  Other people can talk in the thread; the plugin ignores them.
  `@Kandev <answer>` in the thread also works, and it does not create a new task.
- **Answering.** The plugin checks the question is still pending, then sends the answers to Kandev.
  On success it adds ✅ to your reply and posts "Answered — the agent resumes."
- **Answered somewhere else.** If you answer in the Kandev UI, or the question is cancelled or expires,
  the plugin posts a short note in the thread and stops tracking it.
  A late Slack reply gets "already answered" (or "no longer exists"), and nothing is sent.
- **Errors.** If Kandev refuses an answer (for example an invalid option), the plugin says why and you can reply again.
  If Kandev is unreachable, reply again later to retry.

## Why answers go through Kandev's REST route

The plugin Host API can read pending questions. It refuses to answer one unless the request
carries a *human response receipt*. Only Kandev's logged-in web UI can mint that receipt.
A Slack reply never passes through that UI.

So the plugin sends answers to `POST /api/v1/clarification/<pending_id>/respond` on your Kandev.
That route runs the same code as the `answer_question_kandev` MCP tool: the same checks,
the same "first answer wins" rule, and the same resume of the waiting agent.
You authorise it with a Kandev personal access token in the plugin settings.
Answers are recorded as that token's user.

## Setup

1. **Slack app.** Re-apply [`slack-app-manifest.yaml`](../slack-app-manifest.yaml), then reinstall the app.
   Clarification threads need:
   - bot scopes `chat:write` (post the questions), `channels:history` and `groups:history`
     (receive replies in public and private channels), and `reactions:write` (the ✅);
   - bot events `message.channels` and `message.groups`;
   - Socket Mode with the app-level token (`connections:write`).
   The browser-session fallback cannot receive replies, so it cannot run this feature.
2. **Invite the bot** to every channel you map: `/invite @Kandev`.
3. **Kandev token.** If your Kandev has authentication on, create a personal access token
   (`kandev_pat_…`) for the user who should be recorded as answering.
4. **Plugin settings** (Settings → Plugins → Slack):

   | Setting | Key | Example |
   | --- | --- | --- |
   | Post agent questions to Slack | `questions_enabled` | on |
   | Kandev URL | `kandev_url` | `http://127.0.0.1:3040` |
   | Kandev API token | `kandev_api_token` | `kandev_pat_…` (secret; empty if auth is off) |
   | Public Kandev URL (optional) | `kandev_public_url` | `https://kandev.example.com` |
   | Default channel | `questions_default_channel` | `C0123456789` |
   | Workspace → channel | `questions_channels` | `Homelab=C0123456789` (id or name, one per line) |
   | Allowed Slack users | `questions_answerers` | `U0123456789` |
   | Check interval (seconds) | `questions_poll_seconds` | `10` (3–300) |

   The plugin also needs the `tasks` and `interactions` read capabilities, declared in `manifest.yaml`.
   Kandev asks you to approve them when you install or update the plugin.

The plugin status (`GET /api/plugins/kandev-plugin-slack/webhooks/status`) has a `questions` block:
`enabled`, `error`, `posted`, `answered`, and `syncedAt`.
A setup problem here never stops task triage.

## Compatibility

Needs Kandev 0.92.0 or later: that release added pending questions to the plugin Host API.
