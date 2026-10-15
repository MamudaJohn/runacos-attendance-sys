// CABV stores public keys and verifies signed WebAuthn responses. The operating
// system chooses face/fingerprint/PIN; biometric samples never enter this code.
function decode(value) {
  const s = value.replace(/-/g, "+").replace(/_/g, "/");
  return Uint8Array.from(atob(s + "=".repeat((4 - s.length % 4) % 4)), c => c.charCodeAt(0));
}
function encode(value) {
  return btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
function serialize(credential) {
  if (!credential) throw new Error("Phone identity confirmation was cancelled.");
  const r = credential.response;
  const response = {clientDataJSON: encode(r.clientDataJSON)};
  if (r.attestationObject) {
    response.attestationObject = encode(r.attestationObject);
    response.transports = typeof r.getTransports === "function" ? r.getTransports() : [];
  } else {
    response.authenticatorData = encode(r.authenticatorData);
    response.signature = encode(r.signature);
    response.userHandle = r.userHandle ? encode(r.userHandle) : null;
  }
  return {id: credential.id, rawId: encode(credential.rawId), type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment || "platform",
    response, clientExtensionResults: credential.getClientExtensionResults()};
}
async function post(path, csrf, data) {
  const response = await fetch(path, {method: "POST", credentials: "same-origin", redirect: "error",
    headers: {"Content-Type": "application/json", "X-CSRF-Token": csrf}, body: JSON.stringify(data)});
  if (!response.ok) throw new Error((await response.text()).trim() || "Phone identity request failed.");
  return response.json();
}
async function requirePlatform() {
  if (!globalThis.isSecureContext || !globalThis.PublicKeyCredential || !navigator.credentials) {
    throw new Error("Phone identity confirmation needs a trusted HTTPS address and a browser supporting passkeys.");
  }
  if (typeof PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable === "function" &&
      !await PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable()) {
    throw new Error("Set up a phone screen lock and a platform authenticator before confirming attendance.");
  }
}
function creationOptions(json) {
  const options = structuredClone(json);
  options.challenge = decode(options.challenge);
  options.user.id = decode(options.user.id);
  options.excludeCredentials = (options.excludeCredentials || []).map(c => ({...c, id: decode(c.id)}));
  return options;
}
function assertionOptions(json) {
  const options = structuredClone(json);
  options.challenge = decode(options.challenge);
  options.allowCredentials = (options.allowCredentials || []).map(c => ({...c, id: decode(c.id)}));
  return options;
}
export async function identityStatus() {
  const response = await fetch("/api/v1/identity", {credentials: "same-origin", redirect: "error"});
  if (!response.ok) throw new Error((await response.text()).trim());
  return response.json();
}
export async function registerAuthenticator(csrf, password) {
  await requirePlatform();
  const options = await post("/api/v1/identity/register/options", csrf, {password});
  const credential = await navigator.credentials.create({publicKey: creationOptions(options.public_key)});
  return post("/api/v1/identity/register/finish", csrf,
    {transaction_id: options.transaction_id, credential: serialize(credential)});
}
export async function confirmAttendance(sessionID, nonce, csrf) {
  await requirePlatform();
  const base = "/api/v1/attendance/sessions/" + encodeURIComponent(sessionID) + "/identity";
  const options = await post(base + "/options", csrf, {nonce});
  const credential = await navigator.credentials.get({publicKey: assertionOptions(options.public_key)});
  return post(base + "/finish", csrf, {transaction_id: options.transaction_id, credential: serialize(credential)});
}
export async function revokeAuthenticator(credentialID, csrf, password) {
  return post("/api/v1/identity/credentials/" + encodeURIComponent(credentialID) + "/revoke", csrf, {password});
}
