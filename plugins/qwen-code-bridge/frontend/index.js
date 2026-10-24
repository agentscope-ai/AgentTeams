/* Qwen Code bridge — console page (host plugin frontend bundle).
 *
 * Registers a "Qwen Code" route in the QwenPaw console and embeds the local
 * Qwen Code web console through the bridge's reverse proxy. The Qwen Code web
 * shell natively accepts ?theme=dark|light, so the iframe follows QwenPaw's
 * theme (preference key "qwenpaw-theme" + prefers-color-scheme fallback).
 */
(function () {
  var H = window.QwenPaw || {};
  var HOST = H.host || {};
  var React = HOST.React || H.React;
  if (!React || typeof H.registerRoutes !== "function") {
    console.warn("[qwen-code-bridge] host API unavailable; route not registered");
    return;
  }

  var THEME_KEY = "qwenpaw-theme";
  var BASE = "/api/qwen-code-bridge/web/";

  function isDark() {
    var pref = "system";
    try {
      pref = localStorage.getItem(THEME_KEY) || "system";
    } catch (e) {
      pref = "system";
    }
    if (pref === "dark") return true;
    if (pref === "light") return false;
    return !!(
      window.matchMedia &&
      window.matchMedia("(prefers-color-scheme: dark)").matches
    );
  }

  function useHostTheme() {
    try {
      if (HOST.useTheme) return HOST.useTheme() === "dark";
    } catch (e) {}
    return isDark();
  }

  function QwenCodePage() {
    var hostDark = useHostTheme();
    var st = React.useState(hostDark);
    var dark = st[0];
    var setDark = st[1];

    React.useEffect(function () {
      function sync() {
        var next = hostDark;
        setDark(function (prev) {
          return prev === next ? prev : next;
        });
      }
      var mq = window.matchMedia
        ? window.matchMedia("(prefers-color-scheme: dark)")
        : null;
      window.addEventListener("storage", sync);
      if (mq && mq.addEventListener) mq.addEventListener("change", sync);
      // The host applies theme switches via React context (no DOM event);
      // a cheap poll keeps the iframe in sync without touching the host.
      var timer = setInterval(sync, 1500);
      return function () {
        window.removeEventListener("storage", sync);
        if (mq && mq.removeEventListener) mq.removeEventListener("change", sync);
        clearInterval(timer);
      };
    }, [setDark]);

    var src = BASE + "?theme=" + (dark ? "dark" : "light");
    return React.createElement("iframe", {
      key: dark ? "dark" : "light",
      src: src,
      title: "Qwen Code",
      style: { width: "100%", height: "100%", minHeight: 480, border: 0, display: "block" },
      allow: "clipboard-read; clipboard-write",
    });
  }

  function chatIdFromUrl() {
    var m = window.location.pathname.match(/\/chat\/([0-9a-zA-Z-]+)/);
    if (m) return m[1];
    var q = new URLSearchParams(window.location.search).get("id");
    return q || "";
  }

  // Look up a chat's channel server-side. This was referenced by the wrapper
  // but never defined — any chat render threw ReferenceError and took the
  // whole console down (React error boundary). Keep it total: never throw.
  function selectedAgentId() {
    try {
      var raw =
        sessionStorage.getItem("qwenpaw-agent-storage") ||
        localStorage.getItem("qwenpaw-agent-storage");
      if (!raw) return "";
      var parsed = JSON.parse(raw);
      return (parsed && parsed.state && parsed.state.selectedAgent) || "";
    } catch (e) {
      return "";
    }
  }

  function bridgeChatLookup(chatId) {
    try {
      var headers = { Accept: "application/json" };
      var agent = selectedAgentId();
      if (agent) headers["X-Agent-Id"] = agent;
      try {
        var token = localStorage.getItem("qwenpaw_auth_token");
        if (token) headers.Authorization = "Bearer " + token;
      } catch (e) {}
      return fetch("/api/qwen-code-bridge/chat/" + encodeURIComponent(chatId), {
        credentials: "same-origin",
        headers: headers,
      })
        .then(function (resp) {
          return resp && resp.ok ? resp.json() : null;
        })
        .catch(function () {
          return null;
        });
    } catch (e) {
      return Promise.resolve(null);
    }
  }

  function makeChatWrapper(Inner) {
    return function BridgeChatRoute() {
      var st = React.useState({ id: "", chat: null });
      var state = st[0];
      var setState = st[1];
      var lastId = React.useRef("");
      React.useEffect(function () {
        var alive = true;
        var timer = null;
        function tick() {
          try {
            var sid = chatIdFromUrl();
            var key = sid + "|" + selectedAgentId();
            if (key === lastId.current) return;
            lastId.current = key;
            if (!sid) {
              setState({ id: "", chat: null });
              return;
            }
            bridgeChatLookup(sid)
              .then(function (found) {
                if (alive) setState({ id: sid, chat: found || null });
              })
              .catch(function () {
                if (alive) setState({ id: sid, chat: null });
              });
          } catch (e) {
            // A plugin bug must never brick the host console.
            if (alive) setState({ id: "", chat: null });
          }
        }
        tick();
        timer = setInterval(tick, 800);
        return function () {
          alive = false;
          if (timer) clearInterval(timer);
        };
      }, []);
      if (state.chat && state.chat.channel === "qwen-code-bridge") {
        return React.createElement(QwenCodePage);
      }
      return React.createElement(Inner);
    };
  }
  try {
    H.registerRoutes("qwen-code-bridge", [
      {
        path: "/qwen-code",
        label: "Qwen Code",
        icon: React.createElement("img", {src: 'data:image/svg+xml,%3Csvg%20xmlns%3D%22http%3A%2F%2Fwww.w3.org%2F2000%2Fsvg%22%20width%3D%2224%22%20height%3D%2224%22%20viewBox%3D%2217.5%2027.7%20135.6%20135.6%22%3E%0A%20%20%3Cstyle%3E%40media%20(prefers-color-scheme%3A%20dark)%20%7B%20path%20%7B%20fill%3A%20%238B9BFF%3B%20%7D%20%7D%3C%2Fstyle%3E%0A%20%20%3Cpath%20fill%3D%22%23082DFF%22%20d%3D%22M130.06%2C43.66h-33.37c0-1.42-.37-2.84-1.1-4.11l-4.47-7.74c-1.47-2.54-4.18-4.11-7.12-4.11h-8.93c-2.94%2C0-5.65%2C1.57-7.12%2C4.11l-13.68%2C23.69c-1.47%2C2.54-1.47%2C5.67%2C0%2C8.22l4.47%2C7.74c1.47%2C2.54%2C4.18%2C4.11%2C7.12%2C4.11h64.19c2.94%2C0%2C5.65-1.57%2C7.12-4.11l4.47-7.74c1.47-2.54%2C1.47-5.67%2C0-8.22l-4.47-7.74c-1.47-2.54-4.18-4.11-7.12-4.11Z%22%20%2F%3E%0A%20%20%3Cpath%20fill%3D%22%23082DFF%22%20d%3D%22M19.75%2C83.66l16.68%2C28.9c-1.23.71-2.27%2C1.74-3.01%2C3.01l-4.47%2C7.74c-1.47%2C2.54-1.47%2C5.67%2C0%2C8.22l4.47%2C7.74c1.47%2C2.54%2C4.18%2C4.11%2C7.12%2C4.11h27.35c2.94%2C0%2C5.65-1.57%2C7.12-4.11l4.47-7.74c1.47-2.54%2C1.47-5.67%2C0-8.22l-32.1-55.59c-1.47-2.54-4.18-4.11-7.12-4.11h-8.93c-2.94%2C0-5.65%2C1.57-7.12%2C4.11l-4.47%2C7.74c-1.47%2C2.54-1.47%2C5.67%2C0%2C8.22Z%22%20%2F%3E%0A%20%20%3Cpath%20fill%3D%22%23082DFF%22%20d%3D%22M109.55%2C159.19l16.68-28.9c1.23.71%2C2.64%2C1.1%2C4.11%2C1.1h8.93c2.94%2C0%2C5.65-1.57%2C7.12-4.11l4.47-7.74c1.47-2.54%2C1.47-5.67%2C0-8.22l-13.68-23.69c-1.47-2.54-4.18-4.11-7.12-4.11h-8.93c-2.94%2C0-5.65%2C1.57-7.12%2C4.11l-32.1%2C55.59c-1.47%2C2.54-1.47%2C5.67%2C0%2C8.22l4.47%2C7.74c1.47%2C2.54%2C4.18%2C4.11%2C7.12%2C4.11h8.93c2.94%2C0%2C5.65-1.57%2C7.12-4.11Z%22%20%2F%3E%0A%3C%2Fsvg%3E%0A', alt: "Qwen Code", style: {width: 16, height: 16, display: "block"}}),
        priority: 20,
        component: QwenCodePage,
      },
    ]);
    console.info("[plugin:qwen-code-bridge] route /qwen-code registered");
    var disabled = false;
    try {
      disabled =
        localStorage.getItem("qwen-code-bridge-disable-wrap") === "1";
    } catch (e) {}
    if (!disabled && H.route && typeof H.route.wrap === "function") {
      H.route.wrap("qwen-code-bridge", "core.chat", makeChatWrapper);
      console.info("[plugin:qwen-code-bridge] core.chat wrapped");
    } else if (disabled) {
      console.info(
        "[plugin:qwen-code-bridge] chat wrapper disabled by localStorage",
      );
    }
  } catch (err) {
    console.warn("[qwen-code-bridge] registerRoutes failed", err);
  }
})();
