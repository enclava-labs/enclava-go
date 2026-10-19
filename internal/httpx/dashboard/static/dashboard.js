(function () {
  var STORAGE_KEY = 'enclava-dashboard-api-key';
  var THEME_KEY = 'enclava-theme';
  var AUTH_MODE = (document.body && document.body.dataset.dashboardAuthMode) || 'api_key';

  // ── Theme management ──────────────────────────────────────────────

  function initTheme() {
    var stored = localStorage.getItem(THEME_KEY);
    if (stored === 'dark' || (!stored && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
      document.documentElement.classList.add('dark');
    } else if (stored === 'light') {
      document.documentElement.classList.remove('dark');
    }
  }

  window.toggleTheme = function () {
    var isDark = document.documentElement.classList.toggle('dark');
    localStorage.setItem(THEME_KEY, isDark ? 'dark' : 'light');
  };

  // ── Toast notifications ───────────────────────────────────────────

  window.showToast = function (message, type, duration) {
    type = type || 'info';
    duration = duration !== undefined ? duration : 5000;

    var container = document.getElementById('toast-container');
    if (!container) return;

    var toast = document.createElement('div');
    toast.className = 'toast-enter p-4 rounded-lg shadow-lg flex items-center gap-3 min-w-[300px] ' + getToastClasses(type);

    var icon = createToastIcon(type);
    toast.appendChild(icon);

    var msgSpan = document.createElement('span');
    msgSpan.className = 'flex-1 text-sm';
    msgSpan.textContent = message;
    toast.appendChild(msgSpan);

    var closeBtn = document.createElement('button');
    closeBtn.className = 'text-current opacity-70 hover:opacity-100';
    closeBtn.onclick = function () { toast.remove(); };
    var closeSvg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    closeSvg.setAttribute('class', 'h-4 w-4');
    closeSvg.setAttribute('fill', 'none');
    closeSvg.setAttribute('stroke', 'currentColor');
    closeSvg.setAttribute('viewBox', '0 0 24 24');
    var closePath = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    closePath.setAttribute('stroke-linecap', 'round');
    closePath.setAttribute('stroke-linejoin', 'round');
    closePath.setAttribute('stroke-width', '2');
    closePath.setAttribute('d', 'M6 18L18 6M6 6l12 12');
    closeSvg.appendChild(closePath);
    closeBtn.appendChild(closeSvg);
    toast.appendChild(closeBtn);

    container.appendChild(toast);

    if (duration > 0) {
      setTimeout(function () {
        toast.classList.remove('toast-enter');
        toast.classList.add('toast-exit');
        setTimeout(function () { toast.remove(); }, 300);
      }, duration);
    }
  };

  function getToastClasses(type) {
    switch (type) {
      case 'success': return 'bg-green-500 text-white';
      case 'error': return 'bg-destructive text-destructive-foreground';
      case 'warning': return 'bg-yellow-500 text-white';
      default: return 'bg-card text-card-foreground border border-border';
    }
  }

  function createToastIcon(type) {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'h-5 w-5 shrink-0');
    svg.setAttribute('fill', 'none');
    svg.setAttribute('stroke', 'currentColor');
    svg.setAttribute('viewBox', '0 0 24 24');

    var path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('stroke-linecap', 'round');
    path.setAttribute('stroke-linejoin', 'round');
    path.setAttribute('stroke-width', '2');

    var paths = {
      success: 'M5 13l4 4L19 7',
      error: 'M6 18L18 6M6 6l12 12',
      warning: 'M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z',
      info: 'M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z'
    };

    path.setAttribute('d', paths[type] || paths.info);
    svg.appendChild(path);
    return svg;
  }

  // ── API key management ────────────────────────────────────────────

  // Session mode: the server stores the per-session CSRF token and ships it
  // in a script-readable cookie; mutations echo it back in X-CSRF-Token.
  function sessionCSRFToken() {
    var match = document.cookie.match(/(?:^|;\s*)enclava_csrf=([^;]*)/);
    return match ? decodeURIComponent(match[1]) : '';
  }

  function getSavedAPIKey() {
    return localStorage.getItem(STORAGE_KEY) || '';
  }

  function setSavedAPIKey(rawKey) {
    var key = (rawKey || '').trim();
    if (!key) {
      localStorage.removeItem(STORAGE_KEY);
      return;
    }
    localStorage.setItem(STORAGE_KEY, key);
  }

  function formatMaskedKey(key) {
    if (!key) return 'Not set';
    if (key.length <= 12) return key;
    return key.slice(0, 8) + '...' + key.slice(-4);
  }

  function setAuthState(hasKey) {
    var loginPanel = document.getElementById('api-key-login-panel');
    var shell = document.getElementById('dashboard-shell');
    var active = document.getElementById('active-api-key');

    if (!shell) return;

    if (AUTH_MODE === 'session') {
      if (loginPanel) loginPanel.classList.add('hidden');
      shell.classList.remove('hidden');
      if (active) active.textContent = 'Session';
      return;
    }

    if (!loginPanel || !active) return;
    loginPanel.classList.toggle('hidden', hasKey);
    shell.classList.toggle('hidden', !hasKey);
    active.textContent = hasKey ? formatMaskedKey(getSavedAPIKey()) : 'Not set';
  }

  // ── Dashboard messages ────────────────────────────────────────────

  function setMessage(message, tone) {
    var mount = document.getElementById('dashboard-message');
    if (!mount) return;
    if (!message) {
      mount.textContent = '';
      mount.className = 'min-h-[1.5rem] text-sm';
      return;
    }
    var toneClasses = {
      error: 'text-destructive',
      success: 'text-green-600 dark:text-green-400',
      info: 'text-muted-foreground',
      warning: 'text-yellow-600 dark:text-yellow-400',
    };
    var cls = toneClasses[tone] || toneClasses.info;
    mount.textContent = message;
    mount.className = 'min-h-[1.5rem] text-sm ' + cls;
  }

  // ── Service health ────────────────────────────────────────────────

  function setDot(id, tone) {
    var el = document.getElementById(id);
    if (!el) return;
    el.className = 'status-dot' + (tone ? ' ' + tone : '');
  }

  function setSeal(state, title, meta) {
    var band = document.getElementById('seal-band');
    var titleEl = document.getElementById('seal-title');
    var metaEl = document.getElementById('seal-meta');
    if (band) band.setAttribute('data-state', state);
    if (titleEl) titleEl.textContent = title;
    if (metaEl) metaEl.innerHTML = meta;
  }

  function fmtVerifiedAt(iso) {
    if (!iso || iso.indexOf('0001-01-01') === 0) return null;
    var d = new Date(iso);
    if (isNaN(d.getTime())) return null;
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) +
      ' ' + d.toLocaleDateString([], { month: 'short', day: 'numeric' });
  }

  function sep() { return '<span class="sep">·</span>'; }

  function refreshServiceStatus() {
    var systemStatus = document.getElementById('system-status');
    var providerStatus = document.getElementById('provider-status');
    if (!systemStatus || !providerStatus) return;

    fetch('/health')
      .then(function (res) {
        return res.ok ? res.json() : Promise.reject(new Error('health endpoint unavailable'));
      })
      .then(function (payload) {
        var providerState = payload && typeof payload.provider === 'object' ? payload.provider : null;
        var att = payload && typeof payload.attestation === 'object' ? payload.attestation : null;
        var providerName = (att && att.provider) || (providerState && providerState.name) || 'provider';
        var ready = !!(providerState && providerState.ready);

        // System snapshot rows
        systemStatus.textContent = ready ? 'Ready · ' + providerName : 'Not ready · ' + providerName;
        setDot('provider-dot', ready ? 'ok' : 'warn');

        if (att) {
          providerStatus.textContent = att.enabled
            ? (att.verified ? 'Verified' : 'Enabled, not yet verified')
            : 'Not supported by this provider';
          setDot('attest-dot', att.enabled ? (att.verified ? 'ok' : 'warn') : '');
        } else {
          providerStatus.textContent = 'Unavailable';
          setDot('attest-dot', '');
        }

        // Signature seal band
        var verifiedAt = att ? fmtVerifiedAt(att.verified_at) : null;
        var readyTxt = ready ? 'provider live' : 'provider offline';
        if (att && att.enabled && att.verified) {
          setSeal('sealed', 'Enclave sealed',
            'Confidential compute attested' + sep() + providerName +
            (verifiedAt ? sep() + 'verified ' + verifiedAt : '') + sep() + readyTxt);
        } else if (att && att.enabled) {
          setSeal('unsealed', 'Enclave unsealed',
            'Attestation enabled but not yet verified' + sep() + providerName + sep() + readyTxt);
        } else {
          setSeal('off', 'Attestation not active',
            providerName + ' does not expose a confidential attestation' + sep() + readyTxt);
        }
      })
      .catch(function () {
        systemStatus.textContent = 'Could not reach /health';
        setDot('provider-dot', 'warn');
        providerStatus.textContent = '';
        setSeal('off', 'Status unavailable', 'Could not reach the health endpoint.');
      });
  }

  // ── HTMX event handlers ──────────────────────────────────────────

  function getEventPath(event) {
    var d = event.detail || {};
    return (d.path || (d.pathInfo && d.pathInfo.requestPath) || (d.requestConfig && d.requestConfig.path) || '').toString();
  }

  function onBeforeDashboardRequest(event) {
    var path = getEventPath(event);
    if (!path.startsWith('/dashboard/partials/')) return;
    if (AUTH_MODE === 'session') {
      var csrf = sessionCSRFToken();
      if (csrf) {
        event.detail.headers = event.detail.headers || {};
        event.detail.headers['X-CSRF-Token'] = csrf;
      }
      return;
    }

    var key = getSavedAPIKey();
    if (!key) {
      event.preventDefault();
      showToast('Set an API key first to use the dashboard.', 'warning');
      return;
    }

    event.detail.headers = event.detail.headers || {};
    event.detail.headers.Authorization = 'Bearer ' + key;
    event.detail.headers['X-API-Key'] = key;
  }

  function onDashboardError(event) {
    var path = getEventPath(event);
    if (!path.startsWith('/dashboard/')) return;

    var status = event.detail.xhr && event.detail.xhr.status;
    if (status === 401) {
      if (AUTH_MODE === 'session') {
        showToast('Dashboard session expired. Redirecting to login...', 'error');
        window.location.href = '/dashboard/login';
        return;
      }
      showToast('API key is missing or invalid.', 'error');
      return;
    }
    if (status === 403) {
      showToast('API key lacks access to dashboard scopes.', 'error');
      return;
    }
    if (status >= 500) {
      showToast('Server error while loading dashboard sections.', 'error');
      return;
    }
  }

  function onDashboardAfterRequest(event) {
    var path = getEventPath(event);

    // Handle toast messages from response headers
    var toastMessage = event.detail.xhr.getResponseHeader('X-Toast-Message');
    var toastType = event.detail.xhr.getResponseHeader('X-Toast-Type') || 'info';
    if (toastMessage) {
      showToast(toastMessage, toastType);
    }

    if (path === '/dashboard/partials/keys' && event.detail.successful) {
      return;
    }
    if (path === '/dashboard/partials/stats' && event.detail.successful) {
      initializeEndpointCopy();
      refreshServiceStatus();
      return;
    }
  }

  // ── Endpoint copy ─────────────────────────────────────────────────

  function initializeEndpointCopy() {
    var endpoint = document.getElementById('api-endpoint');
    var copyButton = document.getElementById('copy-api-endpoint');
    if (!endpoint) return;

    endpoint.textContent = window.location.origin + '/api/v1';
    if (!copyButton) return;

    copyButton.addEventListener('click', function () {
      navigator.clipboard.writeText(endpoint.textContent || '').then(function () {
        showToast('API endpoint copied to clipboard.', 'success', 3000);
      }).catch(function () {
        showToast('Failed to copy API endpoint.', 'error');
      });
    });
  }

  // ── Initialize ────────────────────────────────────────────────────

  document.body.addEventListener('htmx:configRequest', onBeforeDashboardRequest);
  document.body.addEventListener('htmx:responseError', onDashboardError);
  document.body.addEventListener('htmx:afterRequest', onDashboardAfterRequest);
  document.body.addEventListener('htmx:afterSwap', function () {
    initializeEndpointCopy();
    refreshServiceStatus();
  });

  document.addEventListener('DOMContentLoaded', function () {
    initTheme();

    var keyForm = document.getElementById('api-key-form');
    var keyInput = document.getElementById('api-key-input');
    var clearButton = document.getElementById('clear-api-key');

    setAuthState(AUTH_MODE === 'session' || !!getSavedAPIKey());
    refreshServiceStatus();
    initializeEndpointCopy();
    initExtractPage();

    if (AUTH_MODE === 'session') return;

    if (keyForm) {
      keyForm.addEventListener('submit', function (event) {
        event.preventDefault();
        var raw = keyInput ? keyInput.value : '';
        if (!raw.trim()) {
          showToast('Please paste an API key before saving.', 'warning');
          return;
        }
        setSavedAPIKey(raw);
        window.location.reload();
      });
    }

    if (clearButton) {
      clearButton.addEventListener('click', function () {
        setSavedAPIKey('');
        window.location.reload();
      });
    }
  });

  // ── Extract page ──────────────────────────────────────────────────

  function extractFetch(path, options) {
    options = options || {};
    options.headers = options.headers || {};
    if (AUTH_MODE === 'session') {
      options.headers['X-CSRF-Token'] = sessionCSRFToken();
    } else {
      var key = getSavedAPIKey();
      if (key) {
        options.headers['Authorization'] = 'Bearer ' + key;
        options.headers['X-API-Key'] = key;
      }
    }
    return fetch('/dashboard/partials/extract' + path, options);
  }

  function initExtractPage() {
    var tabsNav = document.getElementById('extract-tabs');
    if (!tabsNav) return;

    // Tab switching
    var tabs = tabsNav.querySelectorAll('.extract-tab');
    tabs.forEach(function (tab) {
      tab.addEventListener('click', function () {
        var target = tab.getAttribute('data-tab');
        switchExtractTab(target);
      });
    });

    // Initial data load
    loadExtractTemplatesForSelect();
    loadExtractJobs();
    loadExtractTemplatesGrid();
    loadExtractModels();
    loadExtractSettings();
    initExtractBaseUrl();

    // File upload
    initExtractFileUpload();

    // Process button
    var processBtn = document.getElementById('extract-process-btn');
    if (processBtn) {
      processBtn.addEventListener('click', processDocument);
    }

    // Refresh jobs button
    var refreshBtn = document.getElementById('extract-refresh-jobs');
    if (refreshBtn) {
      refreshBtn.addEventListener('click', loadExtractJobs);
    }

    // Template management buttons
    var newTplBtn = document.getElementById('extract-new-template-btn');
    if (newTplBtn) {
      newTplBtn.addEventListener('click', function () { showTemplateForm(null); });
    }

    var cancelTplBtn = document.getElementById('extract-template-form-cancel');
    if (cancelTplBtn) {
      cancelTplBtn.addEventListener('click', hideTemplateForm);
    }

    var saveTplBtn = document.getElementById('extract-template-form-save');
    if (saveTplBtn) {
      saveTplBtn.addEventListener('click', saveTemplate);
    }

    var resetBtn = document.getElementById('extract-reset-defaults-btn');
    if (resetBtn) {
      resetBtn.addEventListener('click', resetDefaultTemplates);
    }

    // Settings save
    var saveSettingsBtn = document.getElementById('extract-save-settings-btn');
    if (saveSettingsBtn) {
      saveSettingsBtn.addEventListener('click', saveExtractSettings);
    }

    // API tab copy buttons
    initExtractCopyButtons();
  }

  function switchExtractTab(tabName) {
    var tabs = document.querySelectorAll('.extract-tab');
    var contents = document.querySelectorAll('.extract-tab-content');

    tabs.forEach(function (t) {
      var isActive = t.getAttribute('data-tab') === tabName;
      t.classList.toggle('active', isActive);
      t.classList.toggle('border-primary', isActive);
      t.classList.toggle('text-foreground', isActive);
      t.classList.toggle('border-transparent', !isActive);
      t.classList.toggle('text-muted-foreground', !isActive);
    });

    contents.forEach(function (c) {
      c.classList.toggle('hidden', c.id !== 'extract-tab-' + tabName);
    });
  }

  // ── Process Documents tab ─────────────────────────────────────────

  var extractSelectedFile = null;

  function initExtractFileUpload() {
    var dropzone = document.getElementById('extract-dropzone');
    var fileInput = document.getElementById('extract-file-input');
    if (!dropzone || !fileInput) return;

    fileInput.addEventListener('change', function () {
      if (fileInput.files && fileInput.files.length > 0) {
        extractSelectedFile = fileInput.files[0];
        showSelectedFile(extractSelectedFile.name);
      }
    });

    dropzone.addEventListener('dragover', function (e) {
      e.preventDefault();
      dropzone.classList.add('border-primary', 'bg-primary/5');
    });

    dropzone.addEventListener('dragleave', function (e) {
      e.preventDefault();
      dropzone.classList.remove('border-primary', 'bg-primary/5');
    });

    dropzone.addEventListener('drop', function (e) {
      e.preventDefault();
      dropzone.classList.remove('border-primary', 'bg-primary/5');
      if (e.dataTransfer.files && e.dataTransfer.files.length > 0) {
        extractSelectedFile = e.dataTransfer.files[0];
        fileInput.files = e.dataTransfer.files;
        showSelectedFile(extractSelectedFile.name);
      }
    });
  }

  function showSelectedFile(name) {
    var fileNameEl = document.getElementById('extract-file-name');
    var processBtn = document.getElementById('extract-process-btn');
    if (fileNameEl) {
      fileNameEl.textContent = 'Selected: ' + name;
      fileNameEl.classList.remove('hidden');
    }
    if (processBtn) {
      var templateSelect = document.getElementById('extract-template-select');
      processBtn.disabled = !extractSelectedFile || !templateSelect || !templateSelect.value;
    }
  }

  function loadExtractTemplatesForSelect() {
    var select = document.getElementById('extract-template-select');
    if (!select) return;

    extractFetch('/templates').then(function (res) {
      return res.json();
    }).then(function (data) {
      var templates = data.templates || [];
      select.textContent = '';
      if (templates.length === 0) {
        var opt = document.createElement('option');
        opt.value = '';
        opt.textContent = 'No templates available';
        select.appendChild(opt);
        return;
      }
      templates.forEach(function (t) {
        var opt = document.createElement('option');
        opt.value = t.id;
        opt.textContent = t.name || t.id;
        if (t.description) opt.title = t.description;
        select.appendChild(opt);
      });
      updateProcessBtnState();
    }).catch(function () {
      select.textContent = '';
      var opt = document.createElement('option');
      opt.value = '';
      opt.textContent = 'Failed to load templates';
      select.appendChild(opt);
    });

    select.addEventListener('change', updateProcessBtnState);
  }

  function updateProcessBtnState() {
    var processBtn = document.getElementById('extract-process-btn');
    var templateSelect = document.getElementById('extract-template-select');
    if (processBtn) {
      processBtn.disabled = !extractSelectedFile || !templateSelect || !templateSelect.value;
    }
  }

  function processDocument() {
    var processBtn = document.getElementById('extract-process-btn');
    var templateSelect = document.getElementById('extract-template-select');
    var resultsContent = document.getElementById('extract-results-content');

    if (!extractSelectedFile || !templateSelect || !templateSelect.value) {
      showToast('Please select a template and file first.', 'warning');
      return;
    }

    processBtn.disabled = true;
    processBtn.textContent = 'Processing...';

    resultsContent.textContent = '';
    var loadingDiv = document.createElement('div');
    loadingDiv.className = 'flex items-center gap-2 text-sm text-muted-foreground';
    var spinner = document.createElement('div');
    spinner.className = 'spinner border-current';
    loadingDiv.appendChild(spinner);
    loadingDiv.appendChild(document.createTextNode('Processing document...'));
    resultsContent.appendChild(loadingDiv);

    var form = new FormData();
    form.append('template_id', templateSelect.value);
    form.append('file', extractSelectedFile);

    extractFetch('/process', {
      method: 'POST',
      body: form
    }).then(function (res) {
      return res.json().then(function (data) { return { ok: res.ok, data: data }; });
    }).then(function (result) {
      if (!result.ok) {
        resultsContent.textContent = '';
        var errP = document.createElement('div');
        errP.className = 'text-sm text-destructive';
        errP.textContent = result.data.error || 'Processing failed';
        resultsContent.appendChild(errP);
        showToast('Document processing failed.', 'error');
        return;
      }
      renderExtractResults(result.data);
      showToast('Document processed successfully.', 'success');
      loadExtractJobs();
    }).catch(function (err) {
      resultsContent.textContent = '';
      var errP = document.createElement('div');
      errP.className = 'text-sm text-destructive';
      errP.textContent = 'Request failed: ' + err.message;
      resultsContent.appendChild(errP);
      showToast('Document processing request failed.', 'error');
    }).finally(function () {
      processBtn.disabled = false;
      processBtn.textContent = 'Process Document';
      updateProcessBtnState();
    });
  }

  function renderExtractResults(data) {
    var container = document.getElementById('extract-results-content');
    if (!container) return;
    container.textContent = '';

    // Status row
    var statusRow = document.createElement('div');
    statusRow.className = 'flex items-center gap-2 mb-4 flex-wrap';

    var badge = document.createElement('span');
    badge.className = data.success
      ? 'inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-semibold bg-green-100 dark:bg-green-900/20 text-green-800 dark:text-green-200'
      : 'inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-semibold bg-yellow-100 dark:bg-yellow-900/20 text-yellow-800 dark:text-yellow-200';
    badge.textContent = data.success ? 'Success' : 'Completed with errors';
    statusRow.appendChild(badge);

    if (data.model_used) {
      var modelSpan = document.createElement('span');
      modelSpan.className = 'text-xs text-muted-foreground font-mono';
      modelSpan.textContent = data.model_used;
      statusRow.appendChild(modelSpan);
    }
    if (data.processing_time_ms) {
      var timeSpan = document.createElement('span');
      timeSpan.className = 'text-xs text-muted-foreground';
      timeSpan.textContent = data.processing_time_ms + 'ms';
      statusRow.appendChild(timeSpan);
    }
    container.appendChild(statusRow);

    // Validation errors
    if (data.validation_errors && data.validation_errors.length > 0) {
      var errDiv = document.createElement('div');
      errDiv.className = 'mb-3 p-3 rounded-md bg-destructive/10 text-destructive text-sm';
      data.validation_errors.forEach(function (e) {
        var p = document.createElement('p');
        p.textContent = e;
        errDiv.appendChild(p);
      });
      container.appendChild(errDiv);
    }

    // Validation warnings
    if (data.validation_warnings && data.validation_warnings.length > 0) {
      var warnDiv = document.createElement('div');
      warnDiv.className = 'mb-3 p-3 rounded-md bg-yellow-500/10 text-yellow-700 dark:text-yellow-300 text-sm';
      data.validation_warnings.forEach(function (w) {
        var p = document.createElement('p');
        p.textContent = w;
        warnDiv.appendChild(p);
      });
      container.appendChild(warnDiv);
    }

    // Extracted data table
    if (data.data && typeof data.data === 'object') {
      var keys = Object.keys(data.data);
      if (keys.length > 0) {
        var tableWrap = document.createElement('div');
        tableWrap.className = 'rounded-md border overflow-hidden';
        var table = document.createElement('table');
        table.className = 'w-full text-sm';

        var thead = document.createElement('thead');
        thead.className = 'bg-muted/50';
        var headerRow = document.createElement('tr');
        var th1 = document.createElement('th');
        th1.className = 'px-4 py-2 text-left font-medium text-muted-foreground';
        th1.textContent = 'Field';
        var th2 = document.createElement('th');
        th2.className = 'px-4 py-2 text-left font-medium text-muted-foreground';
        th2.textContent = 'Value';
        headerRow.appendChild(th1);
        headerRow.appendChild(th2);
        thead.appendChild(headerRow);
        table.appendChild(thead);

        var tbody = document.createElement('tbody');
        keys.forEach(function (key) {
          var val = data.data[key];
          var tr = document.createElement('tr');
          tr.className = 'border-t';
          var tdKey = document.createElement('td');
          tdKey.className = 'px-4 py-2 font-mono text-xs align-top';
          tdKey.textContent = key;
          var tdVal = document.createElement('td');
          tdVal.className = 'px-4 py-2 text-xs break-all';
          if (typeof val === 'object' && val !== null) {
            var pre = document.createElement('pre');
            pre.className = 'whitespace-pre-wrap';
            pre.textContent = JSON.stringify(val, null, 2);
            tdVal.appendChild(pre);
          } else {
            tdVal.textContent = String(val);
          }
          tr.appendChild(tdKey);
          tr.appendChild(tdVal);
          tbody.appendChild(tr);
        });
        table.appendChild(tbody);
        tableWrap.appendChild(table);
        container.appendChild(tableWrap);
      }
    }

    // Token usage
    if (data.tokens_used > 0) {
      var usageDiv = document.createElement('div');
      usageDiv.className = 'mt-3 flex gap-4 text-xs text-muted-foreground';
      var parts = ['Tokens: ' + data.tokens_used];
      if (data.prompt_tokens_used) parts.push('Prompt: ' + data.prompt_tokens_used);
      if (data.completion_tokens_used) parts.push('Completion: ' + data.completion_tokens_used);
      parts.forEach(function (text) {
        var span = document.createElement('span');
        span.textContent = text;
        usageDiv.appendChild(span);
      });
      container.appendChild(usageDiv);
    }
  }

  // ── Jobs table ────────────────────────────────────────────────────

  function loadExtractJobs() {
    var container = document.getElementById('extract-jobs-table');
    if (!container) return;

    container.textContent = '';
    var loadingDiv = document.createElement('div');
    loadingDiv.className = 'flex items-center gap-2 text-sm text-muted-foreground py-4';
    var spinner = document.createElement('div');
    spinner.className = 'spinner border-current';
    loadingDiv.appendChild(spinner);
    loadingDiv.appendChild(document.createTextNode('Loading jobs...'));
    container.appendChild(loadingDiv);

    extractFetch('/jobs?limit=20').then(function (res) {
      return res.json();
    }).then(function (data) {
      var jobs = data.jobs || [];
      container.textContent = '';

      if (jobs.length === 0) {
        var emptyP = document.createElement('p');
        emptyP.className = 'text-sm text-muted-foreground py-4';
        emptyP.textContent = 'No jobs yet. Process a document to see results here.';
        container.appendChild(emptyP);
        return;
      }

      var tableWrap = document.createElement('div');
      tableWrap.className = 'rounded-md border overflow-hidden';
      var table = document.createElement('table');
      table.className = 'w-full text-sm';

      var thead = document.createElement('thead');
      thead.className = 'bg-muted/50 [&_tr]:border-b';
      var headerRow = document.createElement('tr');
      ['ID', 'File', 'Template', 'Status', 'Model', 'Created'].forEach(function (label) {
        var th = document.createElement('th');
        th.className = 'h-10 px-4 text-left font-medium text-muted-foreground';
        th.textContent = label;
        headerRow.appendChild(th);
      });
      thead.appendChild(headerRow);
      table.appendChild(thead);

      var tbody = document.createElement('tbody');
      tbody.className = '[&_tr:last-child]:border-0';
      jobs.forEach(function (job) {
        var tr = document.createElement('tr');
        tr.className = 'border-b hover:bg-muted/50 transition-colors';

        var tdId = document.createElement('td');
        tdId.className = 'px-4 py-2.5 font-mono text-xs';
        tdId.textContent = (job.id || '').slice(0, 16);
        tr.appendChild(tdId);

        var tdFile = document.createElement('td');
        tdFile.className = 'px-4 py-2.5 text-xs';
        tdFile.textContent = job.file_name || '-';
        tr.appendChild(tdFile);

        var tdTpl = document.createElement('td');
        tdTpl.className = 'px-4 py-2.5 font-mono text-xs';
        tdTpl.textContent = job.template_id || '-';
        tr.appendChild(tdTpl);

        var tdStatus = document.createElement('td');
        tdStatus.className = 'px-4 py-2.5';
        tdStatus.appendChild(createJobStatusBadge(job.status));
        tr.appendChild(tdStatus);

        var tdModel = document.createElement('td');
        tdModel.className = 'px-4 py-2.5 font-mono text-xs';
        tdModel.textContent = job.model_used || '-';
        tr.appendChild(tdModel);

        var tdCreated = document.createElement('td');
        tdCreated.className = 'px-4 py-2.5 text-xs text-muted-foreground';
        tdCreated.textContent = formatTime(job.created_at);
        tr.appendChild(tdCreated);

        tbody.appendChild(tr);
      });
      table.appendChild(tbody);
      tableWrap.appendChild(table);
      container.appendChild(tableWrap);

      if (data.total > 20) {
        var moreP = document.createElement('p');
        moreP.className = 'text-xs text-muted-foreground mt-2';
        moreP.textContent = 'Showing 20 of ' + data.total + ' jobs.';
        container.appendChild(moreP);
      }
    }).catch(function () {
      container.textContent = '';
      var errP = document.createElement('p');
      errP.className = 'text-sm text-destructive py-4';
      errP.textContent = 'Failed to load jobs.';
      container.appendChild(errP);
    });
  }

  function createJobStatusBadge(status) {
    var classes = {
      completed: 'bg-green-100 dark:bg-green-900/20 text-green-800 dark:text-green-200',
      completed_with_errors: 'bg-yellow-100 dark:bg-yellow-900/20 text-yellow-800 dark:text-yellow-200',
      failed: 'bg-red-100 dark:bg-red-900/20 text-red-800 dark:text-red-200',
      processing: 'bg-blue-100 dark:bg-blue-900/20 text-blue-800 dark:text-blue-200',
      pending: 'bg-gray-100 dark:bg-gray-900/20 text-gray-800 dark:text-gray-200'
    };
    var cls = classes[status] || classes.pending;
    var badge = document.createElement('span');
    badge.className = 'inline-flex items-center rounded-full px-2 py-0.5 text-xs font-semibold ' + cls;
    badge.textContent = (status || 'unknown').replace(/_/g, ' ');
    return badge;
  }

  // ── Templates tab ─────────────────────────────────────────────────

  var extractEditingTemplateId = null;

  function loadExtractTemplatesGrid() {
    var grid = document.getElementById('extract-templates-grid');
    if (!grid) return;

    grid.textContent = '';
    var loadingDiv = document.createElement('div');
    loadingDiv.className = 'flex items-center gap-2 text-sm text-muted-foreground py-4';
    var spinner = document.createElement('div');
    spinner.className = 'spinner border-current';
    loadingDiv.appendChild(spinner);
    loadingDiv.appendChild(document.createTextNode('Loading templates...'));
    grid.appendChild(loadingDiv);

    extractFetch('/templates').then(function (res) {
      return res.json();
    }).then(function (data) {
      var templates = data.templates || [];
      grid.textContent = '';

      if (templates.length === 0) {
        var emptyP = document.createElement('p');
        emptyP.className = 'text-sm text-muted-foreground col-span-3';
        emptyP.textContent = 'No templates configured. Click "New Template" or "Reset Defaults" to get started.';
        grid.appendChild(emptyP);
        return;
      }

      templates.forEach(function (t) {
        var card = document.createElement('div');
        card.className = 'rounded-lg border bg-card text-card-foreground shadow-sm p-5 space-y-3';

        // Header row
        var headerDiv = document.createElement('div');
        headerDiv.className = 'flex items-start justify-between gap-2';
        var nameDiv = document.createElement('div');
        nameDiv.className = 'min-w-0';
        var nameH4 = document.createElement('h4');
        nameH4.className = 'font-semibold text-sm truncate';
        nameH4.title = t.id;
        nameH4.textContent = t.name || t.id;
        var idP = document.createElement('p');
        idP.className = 'text-xs font-mono text-muted-foreground truncate';
        idP.textContent = t.id;
        nameDiv.appendChild(nameH4);
        nameDiv.appendChild(idP);
        headerDiv.appendChild(nameDiv);

        var badgesDiv = document.createElement('div');
        badgesDiv.className = 'flex gap-1 shrink-0';
        if (t.is_default) {
          var defaultBadge = document.createElement('span');
          defaultBadge.className = 'inline-flex items-center rounded-full px-2 py-0.5 text-xs font-semibold bg-blue-100 dark:bg-blue-900/20 text-blue-800 dark:text-blue-200';
          defaultBadge.textContent = 'Default';
          badgesDiv.appendChild(defaultBadge);
        }
        var activeBadge = document.createElement('span');
        activeBadge.className = t.is_active
          ? 'inline-flex items-center rounded-full px-2 py-0.5 text-xs font-semibold bg-green-100 dark:bg-green-900/20 text-green-800 dark:text-green-200'
          : 'inline-flex items-center rounded-full px-2 py-0.5 text-xs font-semibold bg-gray-100 dark:bg-gray-900/20 text-gray-800 dark:text-gray-200';
        activeBadge.textContent = t.is_active ? 'Active' : 'Inactive';
        badgesDiv.appendChild(activeBadge);
        headerDiv.appendChild(badgesDiv);
        card.appendChild(headerDiv);

        // Description
        if (t.description) {
          var descP = document.createElement('p');
          descP.className = 'text-xs text-muted-foreground line-clamp-2';
          descP.textContent = t.description;
          card.appendChild(descP);
        }

        // Model
        if (t.model) {
          var modelP = document.createElement('p');
          modelP.className = 'text-xs font-mono text-muted-foreground';
          modelP.textContent = 'Model: ' + t.model;
          card.appendChild(modelP);
        }

        // Action buttons
        var actionsDiv = document.createElement('div');
        actionsDiv.className = 'flex gap-2 pt-1';
        var editBtn = document.createElement('button');
        editBtn.className = 'inline-flex items-center justify-center rounded-md text-xs font-medium h-7 px-3 border border-input bg-background hover:bg-accent hover:text-accent-foreground transition-colors';
        editBtn.textContent = 'Edit';
        editBtn.addEventListener('click', function () { editTemplate(t.id); });
        var deleteBtn = document.createElement('button');
        deleteBtn.className = 'inline-flex items-center justify-center rounded-md text-xs font-medium h-7 px-3 border border-destructive/30 text-destructive hover:bg-destructive/10 transition-colors';
        deleteBtn.textContent = 'Delete';
        deleteBtn.addEventListener('click', function () { deleteTemplate(t.id); });
        actionsDiv.appendChild(editBtn);
        actionsDiv.appendChild(deleteBtn);
        card.appendChild(actionsDiv);

        grid.appendChild(card);
      });
    }).catch(function () {
      grid.textContent = '';
      var errP = document.createElement('p');
      errP.className = 'text-sm text-destructive col-span-3';
      errP.textContent = 'Failed to load templates.';
      grid.appendChild(errP);
    });
  }

  function showTemplateForm(template) {
    var wrapper = document.getElementById('extract-template-form-wrapper');
    var title = document.getElementById('extract-template-form-title');
    var idField = document.getElementById('tpl-id');
    if (!wrapper) return;

    wrapper.classList.remove('hidden');

    if (template) {
      extractEditingTemplateId = template.id;
      title.textContent = 'Edit Template';
      idField.value = template.id;
      idField.disabled = true;
      document.getElementById('tpl-name').value = template.name || '';
      document.getElementById('tpl-description').value = template.description || '';
      document.getElementById('tpl-system-prompt').value = template.system_prompt || '';
      document.getElementById('tpl-user-prompt').value = template.user_prompt || '';
      var modelSelect = document.getElementById('tpl-model');
      if (modelSelect) {
        modelSelect.value = template.model || '';
      }
    } else {
      extractEditingTemplateId = null;
      title.textContent = 'New Template';
      idField.value = '';
      idField.disabled = false;
      document.getElementById('tpl-name').value = '';
      document.getElementById('tpl-description').value = '';
      document.getElementById('tpl-system-prompt').value = '';
      document.getElementById('tpl-user-prompt').value = '';
      var modelSelectNew = document.getElementById('tpl-model');
      if (modelSelectNew) modelSelectNew.value = '';
    }

    wrapper.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  function hideTemplateForm() {
    var wrapper = document.getElementById('extract-template-form-wrapper');
    if (wrapper) wrapper.classList.add('hidden');
    extractEditingTemplateId = null;
  }

  function editTemplate(templateId) {
    extractFetch('/templates').then(function (res) {
      return res.json();
    }).then(function (data) {
      var templates = data.templates || [];
      var found = null;
      for (var i = 0; i < templates.length; i++) {
        if (templates[i].id === templateId) { found = templates[i]; break; }
      }
      if (found) {
        showTemplateForm(found);
      } else {
        showToast('Template not found.', 'error');
      }
    }).catch(function () {
      showToast('Failed to load template for editing.', 'error');
    });
  }

  function saveTemplate() {
    var idField = document.getElementById('tpl-id');
    var templateId = (idField.value || '').trim();
    var payload = {
      id: templateId,
      name: (document.getElementById('tpl-name').value || '').trim(),
      description: (document.getElementById('tpl-description').value || '').trim(),
      system_prompt: (document.getElementById('tpl-system-prompt').value || '').trim(),
      user_prompt: (document.getElementById('tpl-user-prompt').value || '').trim(),
      model: (document.getElementById('tpl-model').value || '').trim(),
      is_active: true,
      context_schema: {}
    };

    if (!payload.id || !payload.name || !payload.system_prompt || !payload.user_prompt) {
      showToast('Template ID, Name, System Prompt, and User Prompt are required.', 'warning');
      return;
    }

    var isEdit = !!extractEditingTemplateId;
    var url = isEdit ? '/templates/' + encodeURIComponent(extractEditingTemplateId) : '/templates';
    var method = isEdit ? 'PUT' : 'POST';

    extractFetch(url, {
      method: method,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function (res) {
      return res.json().then(function (data) { return { ok: res.ok, data: data }; });
    }).then(function (result) {
      if (!result.ok) {
        showToast(result.data.error || 'Failed to save template.', 'error');
        return;
      }
      showToast('Template saved successfully.', 'success');
      hideTemplateForm();
      loadExtractTemplatesGrid();
      loadExtractTemplatesForSelect();
    }).catch(function () {
      showToast('Failed to save template.', 'error');
    });
  }

  function deleteTemplate(templateId) {
    if (!confirm('Delete template "' + templateId + '"? This cannot be undone.')) return;

    extractFetch('/templates/' + encodeURIComponent(templateId), {
      method: 'DELETE'
    }).then(function (res) {
      return res.json().then(function (data) { return { ok: res.ok, data: data }; });
    }).then(function (result) {
      if (!result.ok) {
        showToast(result.data.error || 'Failed to delete template.', 'error');
        return;
      }
      showToast('Template deleted.', 'success');
      loadExtractTemplatesGrid();
      loadExtractTemplatesForSelect();
    }).catch(function () {
      showToast('Failed to delete template.', 'error');
    });
  }

  function resetDefaultTemplates() {
    if (!confirm('Reset to default templates? This will delete all default templates and recreate them.')) return;

    extractFetch('/reset-defaults', {
      method: 'POST'
    }).then(function (res) {
      return res.json().then(function (data) { return { ok: res.ok, data: data }; });
    }).then(function (result) {
      if (!result.ok) {
        showToast(result.data.error || 'Failed to reset templates.', 'error');
        return;
      }
      showToast('Default templates restored.', 'success');
      loadExtractTemplatesGrid();
      loadExtractTemplatesForSelect();
    }).catch(function () {
      showToast('Failed to reset templates.', 'error');
    });
  }

  // ── Settings tab ──────────────────────────────────────────────────

  function loadExtractSettings() {
    var modelSelect = document.getElementById('extract-default-model');
    if (!modelSelect) return;

    extractFetch('/settings').then(function (res) {
      return res.json();
    }).then(function (data) {
      if (data.default_model) {
        modelSelect.setAttribute('data-current', data.default_model);
        // Try to set it if models are already loaded
        if (modelSelect.querySelector('option[value="' + data.default_model + '"]')) {
          modelSelect.value = data.default_model;
        }
      }
    }).catch(function () {
      // Settings may not be available yet
    });
  }

  function loadExtractModels() {
    var selects = [
      document.getElementById('extract-default-model'),
      document.getElementById('tpl-model')
    ];

    extractFetch('/models').then(function (res) {
      return res.json();
    }).then(function (data) {
      var models = data.models || [];
      selects.forEach(function (select) {
        if (!select) return;
        var currentVal = select.getAttribute('data-current') || select.value;
        select.textContent = '';
        var defaultOpt = document.createElement('option');
        defaultOpt.value = '';
        defaultOpt.textContent = select.id === 'tpl-model' ? 'Use default model' : 'Auto-detect';
        select.appendChild(defaultOpt);
        models.forEach(function (m) {
          var opt = document.createElement('option');
          opt.value = m.id;
          opt.textContent = m.name || m.id;
          select.appendChild(opt);
        });
        if (currentVal) select.value = currentVal;
      });
    }).catch(function () {
      selects.forEach(function (select) {
        if (!select) return;
        select.textContent = '';
        var opt = document.createElement('option');
        opt.value = '';
        opt.textContent = 'Failed to load models';
        select.appendChild(opt);
      });
    });
  }

  function saveExtractSettings() {
    var modelSelect = document.getElementById('extract-default-model');
    if (!modelSelect) return;

    var payload = {
      default_model: modelSelect.value || '',
      max_file_size_mb: 0
    };

    extractFetch('/settings', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function (res) {
      return res.json().then(function (data) { return { ok: res.ok, data: data }; });
    }).then(function (result) {
      if (!result.ok) {
        showToast(result.data.error || 'Failed to save settings.', 'error');
        return;
      }
      showToast('Settings saved successfully.', 'success');
    }).catch(function () {
      showToast('Failed to save settings.', 'error');
    });
  }

  // ── API Integration tab ───────────────────────────────────────────

  function initExtractBaseUrl() {
    var baseUrl = document.getElementById('extract-api-base-url');
    var placeholders = document.querySelectorAll('.extract-api-url-placeholder');
    var apiUrl = window.location.origin + '/api/v1/extract';

    if (baseUrl) baseUrl.textContent = apiUrl;
    placeholders.forEach(function (el) { el.textContent = apiUrl; });

    var copyBtn = document.getElementById('extract-copy-base-url');
    if (copyBtn) {
      copyBtn.addEventListener('click', function () {
        navigator.clipboard.writeText(apiUrl).then(function () {
          showToast('Base URL copied to clipboard.', 'success', 3000);
        }).catch(function () {
          showToast('Failed to copy.', 'error');
        });
      });
    }
  }

  function initExtractCopyButtons() {
    document.querySelectorAll('.extract-copy-code').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var targetId = btn.getAttribute('data-target');
        var codeBlock = document.getElementById(targetId);
        if (!codeBlock) return;
        navigator.clipboard.writeText(codeBlock.textContent || '').then(function () {
          showToast('Code copied to clipboard.', 'success', 3000);
        }).catch(function () {
          showToast('Failed to copy.', 'error');
        });
      });
    });
  }

  // ── Utilities ─────────────────────────────────────────────────────

  function formatTime(isoStr) {
    if (!isoStr) return '-';
    try {
      var d = new Date(isoStr);
      if (isNaN(d.getTime())) return isoStr;
      return d.toLocaleString();
    } catch (e) {
      return isoStr;
    }
  }
})();
