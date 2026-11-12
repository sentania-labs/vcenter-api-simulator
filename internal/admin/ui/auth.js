// Welcome (first time: choose the console password) and Sign in. The second
// first-run step, the password tools sign in with, is shown by the console
// itself once this page has signed the person in.
let theme = 'light';
try { theme = localStorage.getItem('vcs-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'); } catch { /* use light */ }
document.documentElement.setAttribute('data-theme', theme === 'dark' ? 'dark' : 'light');

const $ = (id) => document.getElementById(id);
const claim = location.pathname === '/claim';
const password = $('password'), confirm = $('confirm'), submit = $('submit');
const query = new URLSearchParams(location.search);

if (claim) {
  document.title = 'Welcome · vCenter Simulator';
  // a second step follows only when the deployment supplied no account for tools
  fetch('/api/auth/status').then((r) => r.json()).then((s) => { if (s.accountNeeded) $('step').textContent = 'vCenter Simulator · Step 1 of 2'; }).catch(() => {});
  $('title').textContent = 'Welcome. Choose a password for this console';
  $('intro').textContent = 'This console controls the simulator, so it needs a password of its own. You will type it each time you open the console.';
  $('intro').hidden = false;
  $('hint-pw').hidden = false;
  $('row-pw2').hidden = false;
  $('forgot').hidden = true;
  password.autocomplete = 'new-password';
  submit.textContent = 'Continue';
} else if (query.get('expired')) {
  $('notice').textContent = 'You were signed out because the console was left idle. Sign in again.';
  $('notice').hidden = false;
} else if (query.get('out')) {
  $('notice').textContent = 'You are signed out.';
  $('notice').hidden = false;
}

function fail(field, message) {
  const row = $('row-' + field), err = $('err-' + field);
  row.classList.add('has-err');
  err.textContent = message; err.hidden = false;
  if (field === 'pw') $('hint-pw').hidden = true;
  (field === 'pw' ? password : confirm).focus();
}
function clear() {
  for (const f of ['pw', 'pw2']) { $('row-' + f).classList.remove('has-err'); $('err-' + f).hidden = true; $('err-' + f).textContent = ''; }
  if (claim) $('hint-pw').hidden = false;
}
password.addEventListener('input', clear);
confirm.addEventListener('input', clear);

$('form').addEventListener('submit', async (event) => {
  event.preventDefault();
  clear();
  const pw = password.value;
  if (claim) {
    if (pw.length < 8) { fail('pw', `That is ${pw.length} ${pw.length === 1 ? 'character' : 'characters'}. Use at least 8.`); return; }
    if (pw !== confirm.value) { fail('pw2', 'The two passwords are different. Type the same one in both boxes.'); return; }
  } else if (!pw) { fail('pw', 'Type the console password.'); return; }
  const label = submit.textContent;
  submit.disabled = true; submit.textContent = claim ? 'Saving…' : 'Signing in…';
  let message;
  try {
    const response = await fetch('/api/auth/' + (claim ? 'claim' : 'login'), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: pw }) });
    if (response.ok) { location.replace('/'); return; }
    if (response.status === 409) { location.replace('/login'); return; }
    message = response.status === 429 ? 'Too many wrong passwords. Wait a minute and try again.'
      : claim ? 'The console could not save that password. Use 8 to 72 characters and try again.'
        : 'That password is not right. Check it and try again.';
  } catch { message = 'The console cannot reach the simulator. Check that it is still running, then try again.'; }
  submit.disabled = false; submit.textContent = label;
  fail('pw', message);
});
