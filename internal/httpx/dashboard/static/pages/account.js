// Account page module (OA-30): change password, revoke own sessions, confirm
// an OIDC link challenge. Every write goes through Enclava.api (D-17). Form
// failures render inline via .field-error — a rejected current password is a
// field error on the form, never only a toast.
(function () {
  function setError(id, msg) {
    const el = document.getElementById(id);
    if (!el) return;
    el.textContent = msg || '';
    el.hidden = !msg;
  }

  function clearErrors(scope) {
    scope.querySelectorAll('.field-error').forEach(function (el) {
      el.hidden = true;
      el.textContent = '';
    });
  }
  function showRequestError(err) {
    const action = Enclava.errorAction(err);
    Enclava.toast('error', action.text || (err && err.message) || 'Request failed',
      { action: action });
  }


  const passwordForm = document.getElementById('account-password-form');
  if (passwordForm) passwordForm.addEventListener('submit', function (e) {
    e.preventDefault();
    clearErrors(passwordForm);
    const next = passwordForm.elements.password.value;
    if (next !== passwordForm.elements.password_confirm.value) {
      setError('account-confirm-password-error', 'New passwords do not match.');
      return;
    }
    Enclava.api('POST', '/api/v1/identities/me/password', {
      current_password: passwordForm.elements.current_password.value,
      password: next,
    }).then(function () {
      Enclava.toast('success', 'Password updated.');
      passwordForm.reset();
    }).catch(function (err) {
      showRequestError(err);
      if (err && err.status === 403) {
        setError('account-current-password-error', 'Current password is incorrect.');
        return;
      }
      setError('account-new-password-error', (err && err.message) || 'Could not update the password.');
    });
  });

  // Session revoke rides the shared two-step confirm (enclava:confirm) —
  // arming, timing and label restore belong to app.js. Revoking the current
  // session signs this browser out: the session is dead server-side, so the
  // page navigates to login instead of refreshing a fragment it can no
  // longer fetch.
  document.addEventListener('enclava:confirm', function (e) {
    const trigger = e.target;
    if (!(trigger instanceof HTMLElement) || !trigger.dataset.accountRevoke) return;
    Enclava.api('DELETE', '/api/v1/identities/me/sessions/' + encodeURIComponent(trigger.dataset.accountRevoke))
      .then(function () {
        if (trigger.dataset.accountCurrent) {
          Enclava.toast('info', 'This session was signed out.');
          location.assign('/dashboard/login');
          return;
        }
        Enclava.toast('success', 'Session revoked.');
        Enclava.refresh();
      })
      .catch(showRequestError);
  });

  // Link challenge confirm: POST /api/v1/identities/link binds the verified
  // external identity to THIS account only after the password re-proof. The
  // password input carries name="password" (the OA-12/§6.13 fix); success
  // reloads the page so the identities list refreshes and the consumed
  // token leaves the URL.

  const linkForm = document.getElementById('account-link-form');
  if (linkForm) linkForm.addEventListener('submit', function (e) {
    e.preventDefault();
    clearErrors(linkForm);
    Enclava.api('POST', '/api/v1/identities/link', {
      link_token: linkForm.elements.link_token.value,
      password: linkForm.elements.password.value,
    }).then(function () {
      location.assign('/dashboard/account');
    }).catch(function (err) {
      showRequestError(err);
      setError('account-link-error',
        err && err.status === 403
          ? 'Account linking failed — the challenge or credentials are invalid.'
          : (err && err.message) || 'Could not link the sign-in.');
    });
  });
  // Request API access (P-12/UX-M03): one POST to the account route — the
  // store caps it at one open request; success reloads the card into its
  // pending state. Nothing is granted client- or server-side.
  const accessForm = document.getElementById('account-api-access-form');
  if (accessForm) accessForm.addEventListener('submit', function (e) {
    e.preventDefault();
    Enclava.api('POST', '/dashboard/account/api-access-request', {
      note: accessForm.elements.note.value,
    }).then(function () {
      location.reload();
    }).catch(function (err) {
      showRequestError(err);
    });
  });
})();
