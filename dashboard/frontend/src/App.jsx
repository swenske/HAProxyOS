import { useEffect, useRef, useState } from 'react'

// dashboardd serves this SPA and the /api/nodes REST API on the same
// origin (its own -addr, e.g. :8080) - see dashboard/backend/main.go -
// so every call here is a plain same-origin fetch, no CORS needed.
// Each *node's own* dashboard view lives on a completely different
// origin (its own allocated port, see dashboard/backend/internal/
// nodeproxy) - that's a full page navigation (openNode below), not
// something this SPA fetches into itself: that per-node origin requires
// a TLS client certificate the browser negotiates per origin, and a
// self-signed server certificate the user has to click through once -
// neither of those works transparently from a background fetch() call
// on a different origin.
function openNode(node) {
  const url = `https://${window.location.hostname}:${node.port}/`
  window.open(url, '_blank', 'noopener,noreferrer')
}

// Logo reuses public/favicon.svg (brand/favicon/favicon.svg) rather
// than a separate light/dark <picture> pair - that file already
// self-adapts to the browser's color scheme via an embedded
// prefers-color-scheme media query in its own <style>, so a plain
// <img> is enough here.
function Logo({ size = 28 }) {
  return <img src="/favicon.svg" alt="" width={size} height={size} className="logo" />
}

function NodeList({ nodes, onRemove, busy }) {
  if (nodes.length === 0) {
    return <p className="empty">No nodes registered yet - add one below.</p>
  }
  return (
    <table className="nodes">
      <thead>
        <tr>
          <th>Name</th>
          <th>Address</th>
          <th>Port</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {nodes.map((n) => (
          <tr key={n.id}>
            <td>{n.name}</td>
            <td>{n.address}</td>
            <td>{n.port}</td>
            <td className="actions">
              <button onClick={() => openNode(n)}>Open dashboard</button>
              <button
                className="danger"
                disabled={busy}
                onClick={() => onRemove(n.id)}
              >
                Remove
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// PendingList is the Tailscale-style admission queue: a node announced
// itself (see dashboard/backend/register.go's own doc comment for why
// that's a dedicated TLS port, not this one) but isn't reachable until
// a human approves it here - never fully automatic.
function PendingList({ pending, onApprove, onReject, busy }) {
  if (pending.length === 0) {
    return null
  }
  return (
    <>
      <h2>Pending nodes</h2>
      <table className="nodes">
        <thead>
          <tr>
            <th>Name</th>
            <th>Address</th>
            <th>Announced</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {pending.map((p) => (
            <tr key={p.id}>
              <td>{p.name}</td>
              <td>{p.address}</td>
              <td>{new Date(p.announced_at).toLocaleString()}</td>
              <td className="actions">
                <button disabled={busy} onClick={() => onApprove(p.id)}>
                  Approve
                </button>
                <button className="danger" disabled={busy} onClick={() => onReject(p.id)}>
                  Reject
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  )
}

// copyToClipboard tries the Clipboard API first - unavailable in a
// non-secure context (this SPA's own :8080 port is plain HTTP by
// design, see dashboard/backend/main.go's own package doc comment, and
// browsers only expose navigator.clipboard on https:// or localhost) -
// and falls back to selecting the given textarea/input's text so the
// user can still copy it with Ctrl+C/Cmd+C manually.
async function copyToClipboard(ref) {
  const text = ref.current?.value ?? ''
  try {
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // fall through to the select-and-let-the-user-copy fallback below
  }
  ref.current?.select()
  return false
}

// ProvisionInfo surfaces what an operator needs to provision a new node
// with this Controller (`janusctl lifecycle install`'s own
// -controller-address/-controller-ca flags, see cmd/janusctl's usage) -
// GET /api/controller-info returns this dashboard's own suggested
// address (best-effort, see suggestRegisterAddress's own doc comment
// server-side - always worth double-checking against the real network
// before use) and its TLS identity CA cert (the same one a provisioned
// node verifies before ever sending it anything - no trust-on-first-use).
function ProvisionInfo() {
  const [info, setInfo] = useState(null)
  const [error, setError] = useState(null)
  const [copied, setCopied] = useState('')
  const addressRef = useRef(null)
  const caRef = useRef(null)
  const commandRef = useRef(null)

  useEffect(() => {
    fetch('/api/controller-info')
      .then((resp) => {
        if (!resp.ok) throw new Error('failed to load')
        return resp.json()
      })
      .then(setInfo)
      .catch((err) => setError(err.message))
  }, [])

  if (error || !info) return null

  const address = info.address || 'YOUR-CONTROLLER-ADDRESS'
  const command = `janusctl lifecycle install -controller-address ${address} -controller-ca controller-ca.crt DISK BUNDLE_DIR`

  async function copy(ref, label) {
    const ok = await copyToClipboard(ref)
    setCopied(ok ? `${label} copied` : `select the ${label.toLowerCase()} text and press Ctrl+C/Cmd+C`)
    setTimeout(() => setCopied(''), 4000)
  }

  return (
    <details className="add-node">
      <summary>Provision a new node</summary>
      {!info.address && (
        <p className="hint">
          No address could be guessed automatically - set -advertise-address on dashboardd, or
          just fill one in below yourself before copying the command.
        </p>
      )}
      <label>
        Controller address
        <input ref={addressRef} readOnly value={address} onClick={() => copy(addressRef, 'Address')} />
      </label>
      <label>
        Controller CA certificate
        <textarea ref={caRef} readOnly rows={6} value={info.ca_cert_pem} onClick={() => copy(caRef, 'CA certificate')} />
      </label>
      <label>
        Command (fill in DISK and BUNDLE_DIR, and save the CA certificate above as
        controller-ca.crt first)
        <textarea ref={commandRef} readOnly rows={2} value={command} onClick={() => copy(commandRef, 'Command')} />
      </label>
      {copied && <p className="hint">{copied}</p>}
    </details>
  )
}

const emptyPfxForm = { name: '', address: '', pfx_password: '' }
const emptyPemForm = {
  name: '',
  address: '',
  ca_cert_pem: '',
  bootstrap_cert_pem: '',
  bootstrap_key_pem: '',
}

// Two ways to hand the dashboard a one-time bootstrap admin credential
// for a node - see dashboard/backend/main.go's parseAddNodeRequest.
// .pfx upload is the default: the same file a user already imported
// into their OS certificate store to view a node's own per-node page
// (see dashboard/README.md) - no PEM text to copy/paste at all, a hard
// cryptographic requirement (this dashboard needs its own private key
// to keep talking to the node - a browser can never hand over a
// private key, only prove it holds one - see nodeproxy's own package
// doc) that .pfx upload gets as close to "just pick your cert" as that
// requirement allows. Paste-PEM stays available as a fallback for a
// script/CI caller with raw PEM files already in hand (hack/
// qemu-dashboard-test.sh drives that path directly).
function AddNodeForm({ onAdded }) {
  const [mode, setMode] = useState('pfx')
  const [pfxForm, setPfxForm] = useState(emptyPfxForm)
  const [pfxFile, setPfxFile] = useState(null)
  const [pemForm, setPemForm] = useState(emptyPemForm)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const setPfx = (field) => (e) => setPfxForm({ ...pfxForm, [field]: e.target.value })
  const setPem = (field) => (e) => setPemForm({ ...pemForm, [field]: e.target.value })

  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      let resp
      if (mode === 'pfx') {
        if (!pfxFile) throw new Error('choose a .pfx file')
        const body = new FormData()
        body.set('name', pfxForm.name)
        body.set('address', pfxForm.address)
        body.set('pfx_password', pfxForm.pfx_password)
        body.set('pfx', pfxFile)
        // No Content-Type header here on purpose - fetch sets the
        // multipart boundary itself from the FormData body, and
        // setting it manually would omit that boundary.
        resp = await fetch('/api/nodes', { method: 'POST', body })
      } else {
        resp = await fetch('/api/nodes', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(pemForm),
        })
      }
      if (!resp.ok) {
        throw new Error(await resp.text())
      }
      setPfxForm(emptyPfxForm)
      setPfxFile(null)
      setPemForm(emptyPemForm)
      onAdded()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form className="add-node" onSubmit={submit}>
      <h2>Add a node</h2>
      <div className="mode-toggle">
        <label>
          <input type="radio" checked={mode === 'pfx'} onChange={() => setMode('pfx')} />
          Upload .pfx (recommended)
        </label>
        <label>
          <input type="radio" checked={mode === 'pem'} onChange={() => setMode('pem')} />
          Paste PEM (scripts/CI)
        </label>
      </div>

      {mode === 'pfx' ? (
        <>
          <label>
            Name
            <input value={pfxForm.name} onChange={setPfx('name')} required />
          </label>
          <label>
            Address (host:port, the node&apos;s real gRPC endpoint - default :9505)
            <input value={pfxForm.address} onChange={setPfx('address')} required placeholder="10.0.0.5:9505" />
          </label>
          <label>
            Bootstrap credential (.pfx)
            <span className="hint">
              The same file you imported into your browser/OS to view a node&apos;s own page -
              must include the node&apos;s ca.crt bundled in (openssl pkcs12 -export -certfile
              ca.crt ...). Used once, immediately, to issue this dashboard&apos;s own dedicated
              credential - never stored.
            </span>
            <input
              type="file"
              accept=".pfx,.p12"
              onChange={(e) => setPfxFile(e.target.files?.[0] ?? null)}
              required
            />
          </label>
          <label>
            .pfx password
            <input type="password" value={pfxForm.pfx_password} onChange={setPfx('pfx_password')} />
          </label>
        </>
      ) : (
        <>
          <label>
            Name
            <input value={pemForm.name} onChange={setPem('name')} required />
          </label>
          <label>
            Address (host:port, the node&apos;s real gRPC endpoint - default :9505)
            <input value={pemForm.address} onChange={setPem('address')} required placeholder="10.0.0.5:9505" />
          </label>
          <label>
            CA certificate (the node&apos;s own ca.crt - not sensitive)
            <textarea value={pemForm.ca_cert_pem} onChange={setPem('ca_cert_pem')} required rows={4} />
          </label>
          <label>
            Bootstrap admin certificate
            <span className="hint">
              Used once, immediately, to issue this dashboard&apos;s own dedicated credential -
              never stored (see dashboard/backend/internal/nodeproxy&apos;s own design notes).
            </span>
            <textarea value={pemForm.bootstrap_cert_pem} onChange={setPem('bootstrap_cert_pem')} required rows={4} />
          </label>
          <label>
            Bootstrap admin key
            <textarea value={pemForm.bootstrap_key_pem} onChange={setPem('bootstrap_key_pem')} required rows={4} />
          </label>
        </>
      )}

      {error && <p className="error">{error}</p>}
      <button type="submit" disabled={busy}>
        {busy ? 'Adding…' : 'Add node'}
      </button>
    </form>
  )
}

// SetupForm is forced on the very first visit, before anything else in
// the app is reachable (see dashboard/backend/internal/auth's own doc
// comment) - a single admin password, no username, since this is a
// single-operator tool.
function SetupForm({ onDone }) {
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  async function submit(e) {
    e.preventDefault()
    if (password !== confirm) {
      setError('passwords do not match')
      return
    }
    setBusy(true)
    setError(null)
    try {
      const resp = await fetch('/api/auth/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password }),
      })
      if (!resp.ok) throw new Error(await resp.text())
      onDone()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="auth-screen">
      <div className="brand centered">
        <Logo size={48} />
        <h1>Janus Controller</h1>
      </div>
      <form className="add-node" onSubmit={submit}>
        <h2>Set the admin password</h2>
        <p className="hint">
          First run - choose a password for this Controller. There is one admin account.
        </p>
        <label>
          Password (at least 8 characters)
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={8} autoFocus />
        </label>
        <label>
          Confirm password
          <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} required minLength={8} />
        </label>
        {error && <p className="error">{error}</p>}
        <button type="submit" disabled={busy}>
          {busy ? 'Setting up…' : 'Set password and continue'}
        </button>
      </form>
    </main>
  )
}

function LoginForm({ onDone }) {
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const resp = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password }),
      })
      if (!resp.ok) throw new Error(await resp.text())
      onDone()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="auth-screen">
      <div className="brand centered">
        <Logo size={48} />
        <h1>Janus Controller</h1>
      </div>
      <form className="add-node" onSubmit={submit}>
        <h2>Sign in</h2>
        <label>
          Password
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus />
        </label>
        {error && <p className="error">{error}</p>}
        <button type="submit" disabled={busy}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </main>
  )
}

// AuthGate calls /api/auth/status once on load and renders whichever
// screen that says to - the setup form, the login form, or the real
// app (children). Re-checked after a successful setup/login rather
// than just trusting the client-side action, since that's what the
// server itself decided, not a local assumption.
function AuthGate({ children }) {
  const [status, setStatus] = useState(null) // null while loading
  const [error, setError] = useState(null)

  async function refreshStatus() {
    try {
      const resp = await fetch('/api/auth/status')
      if (!resp.ok) throw new Error(await resp.text())
      setStatus(await resp.json())
    } catch (err) {
      setError(err.message)
    }
  }

  useEffect(() => {
    refreshStatus()
  }, [])

  if (error) return <p className="error">{error}</p>
  if (!status) return <p>Loading…</p>
  if (status.setup_required) return <SetupForm onDone={refreshStatus} />
  if (!status.authenticated) return <LoginForm onDone={refreshStatus} />
  return children
}

function MainApp() {
  const [nodes, setNodes] = useState([])
  const [pending, setPending] = useState([])
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  async function refresh() {
    setLoading(true)
    try {
      const [nodesResp, pendingResp] = await Promise.all([fetch('/api/nodes'), fetch('/api/pending')])
      if (!nodesResp.ok) throw new Error(await nodesResp.text())
      if (!pendingResp.ok) throw new Error(await pendingResp.text())
      setNodes((await nodesResp.json()) ?? [])
      setPending((await pendingResp.json()) ?? [])
      setError(null)
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    refresh()
  }, [])

  async function remove(id) {
    setBusy(true)
    try {
      const resp = await fetch(`/api/nodes/${id}`, { method: 'DELETE' })
      if (!resp.ok) throw new Error(await resp.text())
      await refresh()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function approve(id) {
    setBusy(true)
    try {
      const resp = await fetch(`/api/pending/${id}/approve`, { method: 'POST' })
      if (!resp.ok) throw new Error(await resp.text())
      await refresh()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function reject(id) {
    setBusy(true)
    try {
      const resp = await fetch(`/api/pending/${id}/reject`, { method: 'POST' })
      if (!resp.ok) throw new Error(await resp.text())
      await refresh()
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  async function logout() {
    await fetch('/api/auth/logout', { method: 'POST' })
    // A full reload is simplest here: AuthGate's own state doesn't
    // otherwise know a logout just happened, and a reload re-runs its
    // status check from scratch.
    window.location.reload()
  }

  return (
    <main>
      <div className="header-row">
        <div className="brand">
          <Logo />
          <h1>Janus Controller</h1>
        </div>
        <button onClick={logout}>Log out</button>
      </div>
      {error && <p className="error">{error}</p>}
      {!loading && <PendingList pending={pending} onApprove={approve} onReject={reject} busy={busy} />}
      <h2>Nodes</h2>
      {loading ? <p>Loading…</p> : <NodeList nodes={nodes} onRemove={remove} busy={busy} />}
      <AddNodeForm onAdded={refresh} />
      <ProvisionInfo />
    </main>
  )
}

export default function App() {
  return (
    <AuthGate>
      <MainApp />
    </AuthGate>
  )
}
