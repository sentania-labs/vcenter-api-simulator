let theme = 'light';
try { theme = localStorage.getItem('vcs-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'); } catch { /* use light */ }
document.documentElement.setAttribute('data-theme', theme === 'dark' ? 'dark' : 'light');
const claim = location.pathname === '/claim';
const form = document.querySelector('#form');
const password = document.querySelector('#password');
const confirm = document.querySelector('#confirm');
const error = document.querySelector('#error');
const submit = document.querySelector('#submit');
if (claim) {
  document.title = 'Claim console · vCenter API Simulator';
  document.querySelector('#title').textContent = 'Claim this console';
  document.querySelector('#intro').textContent = 'Choose a password to protect access to this console.';
  document.querySelector('#confirm-row').hidden = false;
  password.autocomplete = 'new-password';
  confirm.required = true;
  submit.textContent = 'Claim console';
}
if (!claim && new URLSearchParams(location.search).get('expired')) { error.textContent = 'Your session ended. Sign in again.'; error.hidden = false; }
form.addEventListener('submit', async (event) => {
  event.preventDefault(); error.hidden = true;
  if (claim && password.value !== confirm.value) { error.textContent = 'The passwords do not match. Try again.'; error.hidden = false; return; }
  submit.disabled = true; submit.textContent = claim ? 'Claiming…' : 'Signing in…';
  try {
    const response = await fetch('/api/auth/' + (claim ? 'claim' : 'login'), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: password.value }) });
    if (response.ok) { location.replace('/'); return; }
    if (response.status === 409) { location.replace('/login'); return; }
    error.textContent = response.status === 429 ? 'Too many attempts. Wait a minute and try again.' : claim ? 'Could not claim the console. Check the password and try again.' : 'That password did not work. Try again.';
  } catch { error.textContent = 'Could not reach the console. Check your connection and try again.'; }
  error.hidden = false; submit.disabled = false; submit.textContent = claim ? 'Claim console' : 'Sign in'; password.focus();
});
