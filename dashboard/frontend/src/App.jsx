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

const emptyForm = {
  name: '',
  address: '',
  ca_cert_pem: '',
  bootstrap_cert_pem: '',
  bootstrap_key_pem: '',
}

function AddNodeForm({ onAdded }) {
  const [form, setForm] = useState(emptyForm)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  const set = (field) => (e) => setForm({ ...form, [field]: e.target.value })

  async function submit(e) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const resp = await fetch('/api/nodes', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(form),
      })
      if (!resp.ok) {
        throw new Error(await resp.text())
      }
      setForm(emptyForm)
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
      <label>
        Name
        <input value={form.name} onChange={set('name')} required />
      </label>
      <label>
        Address (host:port, the node&apos;s real gRPC endpoint - default :9505)
        <input value={form.address} onChange={set('address')} required placeholder="10.0.0.5:9505" />
      </label>
      <label>
        CA certificate (the node&apos;s own ca.crt - not sensitive)
        <textarea value={form.ca_cert_pem} onChange={set('ca_cert_pem')} required rows={4} />
      </label>
      <label>
        Bootstrap admin certificate
        <span className="hint">
          Used once, immediately, to issue this dashboard&apos;s own dedicated credential -
          never stored (see dashboard/backend/internal/nodeproxy&apos;s own design notes).
        </span>
        <textarea value={form.bootstrap_cert_pem} onChange={set('bootstrap_cert_pem')} required rows={4} />
      </label>
      <label>
        Bootstrap admin key
        <textarea value={form.bootstrap_key_pem} onChange={set('bootstrap_key_pem')} required rows={4} />
      </label>
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
      <h1>HAProxyOS Dashboard</h1>
      {error && <p className="error">{error}</p>}
      {loading ? <p>Loading…</p> : <NodeList nodes={nodes} onRemove={remove} busy={busy} />}
      <AddNodeForm onAdded={refresh} />
    </main>
  )
}
