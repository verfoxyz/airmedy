UPDATE stream_cache_tasks
   SET state = 'failed', error = 'interrupted'
 WHERE state = 'canceled' AND error = '';
