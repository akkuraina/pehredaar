-- Telling people when an exam they asked about goes live.
--
-- exam_launches: when Pehredaar first saw each exam live (it has enabled
-- sources in sources.yaml). Only interests recorded before that moment
-- get a "now live" message: someone who picks a live exam is subscribed
-- by the survey directly. SSC was live from the start, so nobody is told
-- about it.
CREATE TABLE exam_launches (
    exam    TEXT    PRIMARY KEY,
    live_at INTEGER NOT NULL
);
INSERT INTO exam_launches (exam, live_at) VALUES ('ssc', 0);

-- launch_notices: who has been told about which exam. Written before the
-- message is sent (like deliveries), so nobody is told twice, even if
-- Pehredaar crashes mid-send or the person re-saves the survey.
CREATE TABLE launch_notices (
    subscriber_id INTEGER NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    exam          TEXT    NOT NULL,
    sent_at       INTEGER NOT NULL,
    PRIMARY KEY (subscriber_id, exam)
);
