-- Intentional stops (track switch, shutdown) were recorded as failures. They are
-- canceled, not errors: the tasks page must stop showing them in the "failed" tab.
UPDATE stream_cache_tasks
   SET state = 'canceled', error = ''
 WHERE state = 'failed' AND error = 'interrupted';
