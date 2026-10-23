(function () {
  window.extractionAsyncUI = true;
  var tabs = document.getElementById('extract-tabs');
  if (!tabs) return;
  var selectedFile = null;
  var timer = null;
  var attempts = 0;
  var activeJob = '';
  var result = document.getElementById('extract-results-content');
  var jobs = document.getElementById('extract-jobs-table');
  var formButton = document.getElementById('extract-process-btn');

  function csrf() {
    var match = document.cookie.match(/(?:^|;\s*)enclava_csrf=([^;]*)/);
    return match ? decodeURIComponent(match[1]) : '';
  }
  function request(url, options) {
    options = options || {};
    options.credentials = 'same-origin';
    options.headers = options.headers || {};
    var token = csrf();
    if (token) options.headers['X-CSRF-Token'] = token;
    return fetch(url, options).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        var error = body.error;
        if (error && typeof error === 'object') error = error.message || error.code;
        if (!response.ok) throw new Error(error || body.message || 'Request failed (' + response.status + ')');
        return body;
      });
    });
  }
  function message(text, error) {
    result.textContent = '';
    var p = document.createElement('p');
    p.className = error ? 'text-sm text-destructive' : 'text-sm text-muted-foreground';
    p.setAttribute('role', error ? 'alert' : 'status');
    p.textContent = text;
    result.appendChild(p);
  }
  function button(label, fn, disabled) {
    var b = document.createElement('button');
    b.type = 'button'; b.textContent = label; b.disabled = !!disabled;
    b.className = 'mt-3 mr-2 inline-flex items-center rounded-md border border-input px-3 py-2 text-sm disabled:opacity-50';
    b.addEventListener('click', fn); return b;
  }
  function showDetail(detail) {
    var job = detail.job || detail;
    result.textContent = '';
    var heading = document.createElement('p');
    heading.className = 'text-sm font-medium';
    heading.setAttribute('role', 'status');
    heading.textContent = (job.file_name || 'Processing job') + ': ' + job.status;
    result.appendChild(heading);
    if (job.error_message) { var err = document.createElement('p'); err.className = 'mt-2 text-sm text-destructive'; err.textContent = job.error_message; result.appendChild(err); }
    if (detail.result) {
      var r = detail.result;
      var generated = document.createElement('p'); generated.className = 'mt-3 text-xs text-muted-foreground';
      generated.textContent = 'Generated output' + (r.schema_version ? ' · schema ' + r.schema_version : '') + ' · ' + (r.tokens_used || 0) + ' tokens · ' + (r.cost_micros || 0) + ' cost micros. Location and confidence are unavailable unless supplied by the model.';
      result.appendChild(generated);
      var pre = document.createElement('pre'); pre.className = 'mt-2 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-3 text-sm'; pre.textContent = JSON.stringify(r.data, null, 2); result.appendChild(pre);
      if (r.raw_response) { var raw = document.createElement('details'); raw.className='mt-3'; var summary=document.createElement('summary'); summary.textContent='Raw model response'; raw.appendChild(summary); var rawText=document.createElement('pre'); rawText.className='mt-2 overflow-auto whitespace-pre-wrap break-words rounded bg-muted p-3 text-sm'; rawText.textContent=r.raw_response; raw.appendChild(rawText); result.appendChild(raw); }
      (r.validation_errors || []).forEach(function (text) { var p = document.createElement('p'); p.className='mt-2 text-sm text-destructive'; p.textContent='Validation error: '+text; result.appendChild(p); });
      (r.validation_warnings || []).forEach(function (text) { var p = document.createElement('p'); p.className='mt-2 text-sm text-yellow-700'; p.textContent='Validation warning: '+text; result.appendChild(p); });
      var original = button('Download original evidence', function () { window.location.assign('/api/v1/extract/jobs/' + encodeURIComponent(job.job_id || job.id) + '/original'); }); result.appendChild(original);
    }
    if (job.status === 'pending' || job.status === 'processing' || job.status === 'queued' || job.status === 'running') {
      result.appendChild(button('Cancel job', function () { request('/api/v1/extract/jobs/' + encodeURIComponent(job.job_id || job.id) + '/cancel', {method:'POST'}).then(function(){ message('Cancellation requested.'); refreshJob(job.job_id || job.id); }).catch(function(e){message(e.message,true);}); }));
    }
    result.appendChild(button('Delete job and stored artifacts', function () {
      if (!window.confirm('Delete this job and its stored input and result?')) return;
      request('/api/v1/extract/jobs/' + encodeURIComponent(job.job_id || job.id), {method:'DELETE'}).then(function(){ message('Job and stored artifacts deleted.'); activeJob=''; loadJobs(); }).catch(function(e){message(e.message,true);});
    }));
  }
  function refreshJob(id) {
    if (timer) window.clearTimeout(timer);
    if (attempts >= 30 || document.hidden) { timer = null; return; }
    attempts++;
    timer = window.setTimeout(function () {
      request('/api/v1/extract/jobs/' + encodeURIComponent(id)).then(function(detail){
        showDetail(detail);
        var status = (detail.job || detail).status;
        if (['pending','processing','queued','running'].indexOf(status) >= 0) refreshJob(id);
        else { activeJob=''; loadJobs(); }
      }).catch(function(e){ activeJob=''; timer=null; message(e.message, true); loadJobs(); });
    }, Math.min(1000 * Math.pow(1.35, attempts), 10000));
  }
  function loadJobs() {
    request('/api/v1/extract/jobs?limit=20').then(function(data){
      jobs.textContent='';
      var list=data.jobs || [];
      if (!list.length) { jobs.textContent='No recent processing jobs.'; return; }
      list.forEach(function(job){
        var row=document.createElement('div'); row.className='flex flex-col gap-2 border-t py-3 sm:flex-row sm:items-center sm:justify-between';
        var label=document.createElement('span'); label.className='min-w-0 break-words text-sm'; label.textContent=(job.file_name || job.template_id || job.id)+ ' · '+job.status; row.appendChild(label);
        row.appendChild(button('Inspect', function(){ activeJob=job.job_id || job.id; attempts=0; request('/api/v1/extract/jobs/'+encodeURIComponent(activeJob)).then(function(d){showDetail(d); if(['pending','processing','queued','running'].indexOf((d.job||d).status)>=0)refreshJob(activeJob);}).catch(function(e){message(e.message,true);}); }));
        jobs.appendChild(row);
      });
    }).catch(function(e){ jobs.textContent='Jobs unavailable: '+e.message; });
  }
  document.addEventListener('visibilitychange', function(){ if(document.hidden && timer){clearTimeout(timer); timer=null;} else if(!document.hidden && activeJob) refreshJob(activeJob); });
  window.addEventListener('pagehide', function(){ if(timer) clearTimeout(timer); });
  document.getElementById('extract-file-input').addEventListener('change',function(e){selectedFile=e.target.files[0]||null;document.getElementById('extract-selected-file').textContent=selectedFile?'Selected: '+selectedFile.name:'';formButton.disabled=!selectedFile||!document.getElementById('extract-template-select').value;});
  document.getElementById('extract-dropzone').addEventListener('drop',function(e){if(e.dataTransfer.files[0]){selectedFile=e.dataTransfer.files[0];document.getElementById('extract-selected-file').textContent='Selected: '+selectedFile.name;formButton.disabled=!document.getElementById('extract-template-select').value;}});
  document.getElementById('extract-template-select').addEventListener('change',function(){formButton.disabled=!selectedFile||!this.value;});
  formButton.addEventListener('click',function(){
    var template=document.getElementById('extract-template-select').value;
    if(!selectedFile||!template){message('Choose a template and file.',true);return;}
    var data=new FormData(); data.append('template_id',template); data.append('operation',document.getElementById('extract-operation').value); data.append('file',selectedFile);
    formButton.disabled=true; message('Submitting processing job…');
    request('/api/v1/extract/jobs',{method:'POST',body:data}).then(function(created){
      activeJob=created.job_id; attempts=0; message('Job '+created.job_id+' accepted; status: '+created.status+'.'); loadJobs(); refreshJob(created.job_id);
    }).catch(function(e){message(e.message,true);}).finally(function(){formButton.disabled=!selectedFile||!template;});
  });
  document.getElementById('extract-refresh-jobs').addEventListener('click',loadJobs);
  loadJobs();
})();
