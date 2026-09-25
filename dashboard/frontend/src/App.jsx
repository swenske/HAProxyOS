import { useEffect, useState } from 'react'

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

export default function App() {
  const [nodes, setNodes] = useState([])
  const [loading, setLoading] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  async function refresh() {
    setLoading(true)
    try {
      const resp = await fetch('/api/nodes')
      if (!resp.ok) throw new Error(await resp.text())
      const data = await resp.json()
      setNodes(data ?? [])
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

  return (
    <main>
      <h1>Janus Controller</h1>
      {error && <p className="error">{error}</p>}
      {loading ? <p>Loading…</p> : <NodeList nodes={nodes} onRemove={remove} busy={busy} />}
      <AddNodeForm onAdded={refresh} />
    </main>
  )
}
