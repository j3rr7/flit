(() => {
  const pathMatch = location.pathname.match(/^\/r\/([^/]+)$/);
  const room = pathMatch
    ? decodeURIComponent(pathMatch[1])
    : new URLSearchParams(location.search).get("room");
  if (!room) {
    location.replace("/");
    return;
  }

  let name = localStorage.getItem("flit.name") || "";
  if (!name) {
    name =
      (prompt("Display name?", "anon") || "anon").trim().slice(0, 32) || "anon";
    localStorage.setItem("flit.name", name);
  }

  const editor = document.getElementById("editor");
  const overlay = document.getElementById("cursor-layer");
  const mirror = document.getElementById("mirror");
  const peersEl = document.getElementById("peers");
  const statusEl = document.getElementById("status");
  document.getElementById("room-label").textContent = "# " + room;

  const statusElFlashPrev = { v: null };
  function flashStatus(text, ms = 1500) {
    const prev = statusEl.textContent;
    statusEl.textContent = text;
    setTimeout(() => {
      if (statusEl.textContent === text) statusEl.textContent = prev;
    }, ms);
  }

  document.getElementById("share-btn")?.addEventListener("click", async () => {
    const url = location.href;
    let ok = false;
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(url);
        ok = true;
      }
    } catch {}
    if (!ok) {
      const ta = document.createElement("textarea");
      ta.value = url;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try {
        ok = document.execCommand("copy");
      } catch {}
      document.body.removeChild(ta);
    }
    if (ok) flashStatus("link copied");
    else prompt("Copy this link:", url);
  });


  const mainEl = document.querySelector("main");
  const peersToggle = document.getElementById("peers-toggle");
  const PEERS_KEY = "flit.peersHidden";

  function applyPeersState(hidden) {
    mainEl.classList.toggle("no-peers", hidden);
    if (peersToggle) {
      peersToggle.textContent = hidden ? "Show peers" : "Hide peers";
      peersToggle.title = hidden ? "Show peers" : "Hide peers";
    }
  }

  applyPeersState(localStorage.getItem(PEERS_KEY) === "1");

  peersToggle?.addEventListener("click", () => {
    const hidden = !mainEl.classList.contains("no-peers");
    applyPeersState(hidden);
    localStorage.setItem(PEERS_KEY, hidden ? "1" : "0");
    syncMirrorBox();
    scheduleOverlay();
  });

  const peers = new Map();
  let selfId = null;
  let suppress = false;
  let reconnectDelay = 500;
  let fatal = false;
  let pendingPassword = null;
  let ws = null;

  function syncMirrorBox() {
    const cs = getComputedStyle(editor);
    mirror.style.width =
      editor.clientWidth -
      parseFloat(cs.paddingLeft) -
      parseFloat(cs.paddingRight) +
      "px";
  }
  function escapeHtml(s) {
    return s.replace(
      /[&<>"']/g,
      (ch) =>
        ({
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          '"': "&quot;",
          "'": "&#39;",
        })[ch],
    );
  }
  function indexToLineCol(text, idx) {
    let line = 1,
      col = 1;
    const end = Math.min(idx, text.length);
    for (let i = 0; i < end; i++) {
      if (text.charCodeAt(i) === 10) {
        line++;
        col = 1;
      } else col++;
    }
    return { line, col };
  }
  let overlayPending = false;
  function scheduleOverlay() {
    if (overlayPending) return;
    overlayPending = true;
    requestAnimationFrame(() => {
      overlayPending = false;
      renderCursorOverlay();
    });
  }

  function renderCursorOverlay() {
    if (!selfId) return;
    syncMirrorBox();

    const remote = [...peers.entries()]
      .filter(([id]) => id !== selfId)
      .map(([id, cur]) => ({
        id,
        i: Math.max(0, Math.min(Number(cur.i) || 0, editor.value.length)),
        cur,
      }));

    if (remote.length === 0) {
      overlay.innerHTML = "";
      mirror.innerHTML = "";
      return;
    }

    remote.sort((a, b) => a.i - b.i);
    const text = editor.value;
    let html = "";
    let last = 0;
    for (const r of remote) {
      html += escapeHtml(text.slice(last, r.i));
      html += `<span class="cm" data-id="${escapeHtml(r.id)}"></span>`;
      last = r.i;
    }
    html += escapeHtml(text.slice(last));
    mirror.innerHTML = html;

    // Measure each marker.
    const mirrorRect = mirror.getBoundingClientRect();
    const positions = new Map();
    for (const m of mirror.querySelectorAll(".cm")) {
      const r = m.getBoundingClientRect();
      positions.set(m.dataset.id, {
        top: r.top - mirrorRect.top,
        left: r.left - mirrorRect.left,
      });
    }

    // Place overlay cursors.
    const scrollTop = editor.scrollTop;
    const scrollLeft = editor.scrollLeft;
    overlay.innerHTML = "";
    for (const r of remote) {
      const pos = positions.get(r.id);
      if (!pos) continue;
      const el = document.createElement("div");
      el.className = "rcursor";
      el.style.top = pos.top - scrollTop + "px";
      el.style.left = pos.left - scrollLeft + "px";
      el.style.setProperty("--c", r.cur.k || "#888");

      const bar = document.createElement("div");
      bar.className = "rcursor-bar";
      const label = document.createElement("div");
      label.className = "rcursor-label";
      label.textContent = r.cur.n || "anon";
      el.append(bar, label);
      overlay.appendChild(el);
    }
  }

  function handleJoinError(reason) {
    try {
      ws && ws.close();
    } catch {}
    switch (reason) {
      case "wrong_password": {
        try {
          sessionStorage.removeItem("flit.pw." + room);
        } catch {}
        const pw = prompt("Wrong password. Try again:");
        if (pw !== null) {
          pendingPassword = pw;
          start();
          return;
        }
        statusEl.textContent = "wrong password";
        fatal = true;
        break;
      }
      case "room_not_found":
        statusEl.textContent = "room not found or expired";
        fatal = true;
        if (confirm("Room not found or expired. Create a new one?"))
          location.href = "/";
        break;
      case "rate_limited":
        statusEl.textContent = "too many attempts";
        fatal = true;
        break;
      default:
        statusEl.textContent = "error: " + reason;
        fatal = true;
    }
  }

  async function start() {
    if (fatal) return;
    try {
      const info = await fetch("/api/rooms/" + encodeURIComponent(room)).then(
        (r) => r.json(),
      );
      if (!info.exists) {
        statusEl.textContent = "room not found or expired";
        fatal = true;
        if (confirm("Room not found or expired. Create a new one?"))
          location.href = "/";
        return;
      }
      if (info.hasPassword && pendingPassword === null) {
        let remembered = null;
        try {
          remembered = sessionStorage.getItem("flit.pw." + room);
        } catch {}
        if (remembered !== null) {
          pendingPassword = remembered;
        } else {
          const pw = prompt("This room is locked. Enter password:");
          if (pw === null) {
            statusEl.textContent = "password required";
            fatal = true;
            return;
          }
          pendingPassword = pw;
        }
      }

      if (info.hasPassword) {
        const btn = document.getElementById("lock-btn");
        if (btn) {
          btn.textContent = "Locked";
          btn.disabled = true;
        }
      }
      connect();
    } catch (e) {
      statusEl.textContent = "network error";
      if (!fatal) setTimeout(start, 2000);
    }
  }

  function connect() {
    const proto = location.protocol === "https:" ? "wss" : "ws";
    const url = `${proto}://${location.host}/ws?room=${encodeURIComponent(room)}&name=${encodeURIComponent(name)}`;
    ws = new WebSocket(url);
    statusEl.textContent = "connecting…";

    ws.onopen = () => {
      ws.send(JSON.stringify({ t: 6, p: { w: pendingPassword || "" } }));
    };

    ws.onmessage = (ev) => {
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (msg.t === 1) {
        statusEl.textContent = "connected";
        reconnectDelay = 500;
        pendingPassword = null;
        try {
          sessionStorage.removeItem("flit.pw." + room);
        } catch {}
      }
      if (msg.t === 7) {
        handleJoinError(msg.p.e);
        return;
      }
      handle(msg.t, msg.p);
    };

    ws.onclose = () => {
      if (fatal) return;
      statusEl.textContent = "reconnecting…";
      setTimeout(start, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 5000);
    };

    ws.onerror = () => {
      try {
        ws.close();
      } catch {}
    };
  }

  function handle(type, p) {
    switch (type) {
      case 1: {
        // init
        selfId = p.u;
        peers.clear();
        for (const [id, cur] of Object.entries(p.s || {})) peers.set(id, cur);
        suppress = true;
        editor.value = p.c || "";
        suppress = false;
        renderPeers();
        queueCursor();
        break;
      }
      case 2: {
        // text update
        const oldText = editor.value;
        const newText = p.c || "";
        const s = editor.selectionStart,
          e = editor.selectionEnd;
        let prefix = 0;
        while (
          prefix < oldText.length &&
          prefix < newText.length &&
          oldText[prefix] === newText[prefix]
        )
          prefix++;
        let suffix = 0;
        while (
          suffix < oldText.length - prefix &&
          suffix < newText.length - prefix &&
          oldText[oldText.length - 1 - suffix] ===
            newText[newText.length - 1 - suffix]
        )
          suffix++;
        const oldEnd = oldText.length - suffix;
        const newEnd = newText.length - suffix;
        const delta = newEnd - oldEnd;
        const shift = (idx) =>
          idx <= prefix
            ? idx
            : idx >= oldEnd
              ? Math.max(0, idx + delta)
              : prefix;

        suppress = true;
        editor.value = newText;
        editor.setSelectionRange(shift(s), shift(e));
        suppress = false;

        for (const [, cur] of peers) cur.i = shift(Number(cur.i) || 0);

        renderPeers();
        scheduleOverlay();
        queueCursor();
        break;
      }
      case 3: {
        if (p.g && p.g.u) {
          peers.set(p.g.u, p.g);
          renderPeers();
          scheduleOverlay();
        }
        break;
      }
      case 4: {
        // peer join
        if (p && p.u) {
          peers.set(p.u, p);
          renderPeers();
          scheduleOverlay();
        }
        break;
      }
      case 5: {
        // peer leave
        if (p && p.u) {
          peers.delete(p.u);
          renderPeers();
          scheduleOverlay();
        }
        break;
      }
    }
  }

  function renderPeers() {
    peersEl.innerHTML = "";
    for (const [id, cur] of peers) {
      const el = document.createElement("div");
      el.className = "peer" + (id === selfId ? " self" : "");

      const dot = document.createElement("span");
      dot.className = "dot";
      dot.style.background = cur.k || "#888";

      const label = document.createElement("span");
      label.textContent = (cur.n || "anon") + (id === selfId ? " (you)" : "");

      const pos = document.createElement("span");
      pos.className = "pos";
      const idx = Math.max(
        0,
        Math.min(Number(cur.i) || 0, editor.value.length),
      );
      const { line, col } = indexToLineCol(editor.value, idx);
      pos.textContent = "L" + line + ":" + col;

      el.append(dot, label, pos);
      peersEl.appendChild(el);
    }
  }

  function send(type, payload) {
    if (!ws || ws.readyState !== 1) return;
    ws.send(JSON.stringify({ t: type, p: payload }));
  }

  let textTimer = null,
    cursorTimer = null,
    pendingCursor = null;
  function queueText() {
    if (suppress) return;
    scheduleOverlay();
    clearTimeout(textTimer);
    textTimer = setTimeout(() => send(2, { c: editor.value }), 80);
  }
  function queueCursor() {
    if (suppress) return;
    pendingCursor = editor.selectionStart;
    if (cursorTimer) return;
    cursorTimer = setTimeout(() => {
      cursorTimer = null;
      send(3, { g: { i: pendingCursor } });
    }, 60);
  }
  editor.addEventListener("input", queueText);
  editor.addEventListener("keyup", queueCursor);
  editor.addEventListener("click", queueCursor);
  editor.addEventListener("select", queueCursor);
  editor.addEventListener("scroll", scheduleOverlay);

  window.addEventListener("resize", () => {
    syncMirrorBox();
    scheduleOverlay();
  });

  document.getElementById("lock-btn")?.addEventListener("click", async () => {
    const btn = document.getElementById("lock-btn");
    if (btn.disabled) return;
    const pw = prompt("Set a password for this room:");
    if (!pw) return;
    btn.disabled = true;
    try {
      const res = await fetch(
        "/api/rooms/" + encodeURIComponent(room) + "/password",
        {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ password: pw }),
        },
      );
      if (res.status === 409) {
        statusEl.textContent = "room already locked";
        btn.textContent = "Locked";
        return;
      }
      if (!res.ok) {
        alert("Failed to lock room");
        btn.disabled = false;
        return;
      }
      btn.textContent = "Locked";
      statusEl.textContent = "locked";
    } catch {
      alert("Network error");
      btn.disabled = false;
    }
  });

  syncMirrorBox();
  start();
  editor.focus();
})();
