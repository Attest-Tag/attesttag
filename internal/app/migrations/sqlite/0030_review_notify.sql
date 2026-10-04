-- Code review's announcements in a chat channel: one message per pull request, posted at its first
-- review or its merge and edited in place after that, with each later event a reply in its thread.
--
--   review_prs.notify_team, notify_channel, notify_ts
--                     where that message is: the workspace (a teams.team_id), the channel and the
--                     message's own id (a Slack ts, a Teams activity id). Empty until the first
--                     announcement. A channel changed since in the settings gets a message of its
--                     own, and one somebody deleted is posted again at the next event; both write
--                     here, conditional on the id they read, so two events racing post one message.
--   review_prs.notify_last
--                     the last event announced: "run:<public id>" for a review, "merged" for the
--                     merge. A redelivered merge, or a review announced once already, is not said
--                     twice in the thread.
alter table review_prs add column notify_team text not null default '';
alter table review_prs add column notify_channel text not null default '';
alter table review_prs add column notify_ts text not null default '';
alter table review_prs add column notify_last text not null default '';
