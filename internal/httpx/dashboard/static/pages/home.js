/* home.js — Home page module: the ask-box handoff (layout "ask").
 *
 * The composer (#homeAsk) is a plain GET form to /dashboard/chat; this module
 * upgrades it: submit (button or Enter without Shift in #homeAskText) trims
 * the draft — empty refocuses and stops. A checked data-search-only target
 * navigates to /dashboard/search?q=…; an assistant target stores the draft in
 * sessionStorage under 'enclava:chat:draft' ({assistant, text} — consumed by
 * the chat page) and navigates to /dashboard/chat?assistant=<id>. The message
 * text never goes into the URL.
 */
(function () {
  if (window.__enclavaHome) return;
  window.__enclavaHome = true;

  const DRAFT_KEY = 'enclava:chat:draft';

  function box() { return document.getElementById('homeAsk'); }

  function submitAsk() {
    const form = box();
    const field = document.getElementById('homeAskText');
    if (!form || !field) return;
    const text = field.value.trim();
    if (!text) { field.focus(); return; }
    const chosen = form.querySelector('input[name="assistant"]:checked');
    if (chosen && chosen.hasAttribute('data-search-only')) {
      location.href = '/dashboard/search?q=' + encodeURIComponent(text);
      return;
    }
    const assistant = chosen ? chosen.value : '';
    try {
      sessionStorage.setItem(DRAFT_KEY, JSON.stringify({ assistant: assistant, text: text }));
    } catch (e) { /* storage unavailable (private mode/quota): navigate without the draft */ }
    location.href = '/dashboard/chat?assistant=' + encodeURIComponent(assistant);
  }

  document.addEventListener('submit', (e) => {
    if (e.target instanceof Element && e.target.id === 'homeAsk') {
      e.preventDefault();
      submitAsk();
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' || e.shiftKey) return;
    if (e.target instanceof Element && e.target.id === 'homeAskText') {
      e.preventDefault();
      submitAsk();
    }
  });
})();
