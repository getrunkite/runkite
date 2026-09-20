/* Docs handbook helpers: copy blocks, keep sidebar scroll, pending badges. */
(function () {
  var SIDEBAR_KEY = "runkite-docs-nav-scroll";

  function ready(fn) {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", fn);
    } else {
      fn();
    }
  }

  function enhanceCodeBlocks() {
    var blocks = document.querySelectorAll("pre.mono-block");
    for (var i = 0; i < blocks.length; i++) {
      var pre = blocks[i];
      if (pre.parentElement && pre.parentElement.classList.contains("code-block")) continue;
      var wrap = document.createElement("div");
      wrap.className = "code-block";
      pre.parentNode.insertBefore(wrap, pre);
      wrap.appendChild(pre);

      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "copy-btn";
      btn.textContent = "Copy";
      btn.setAttribute("aria-label", "Copy code");
      wrap.appendChild(btn);

      btn.addEventListener("click", function (ev) {
        var target = ev.currentTarget;
        var code = target.parentElement.querySelector("pre");
        var text = code ? code.textContent : "";
        function ok() {
          target.textContent = "Copied";
          target.classList.add("is-copied");
          setTimeout(function () {
            target.textContent = "Copy";
            target.classList.remove("is-copied");
          }, 1400);
        }
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(text).then(ok).catch(function () {
            fallbackCopy(text, ok);
          });
        } else {
          fallbackCopy(text, ok);
        }
      });
    }
  }

  function fallbackCopy(text, ok) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.left = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand("copy");
      ok();
    } catch (_) {}
    document.body.removeChild(ta);
  }

  function keepSidebarScroll() {
    var sidebar = document.querySelector(".sidebar");
    if (!sidebar) return;

    var saved = sessionStorage.getItem(SIDEBAR_KEY);
    if (saved !== null) {
      sidebar.scrollTop = parseInt(saved, 10) || 0;
    }

    var active = sidebar.querySelector("a.active");
    if (active) {
      // Keep the current item in view without yanking the list to the top.
      try {
        active.scrollIntoView({ block: "nearest", inline: "nearest" });
      } catch (_) {
        /* older browsers */
      }
    }

    function persist() {
      sessionStorage.setItem(SIDEBAR_KEY, String(sidebar.scrollTop));
    }
    sidebar.addEventListener("scroll", persist, { passive: true });
    var links = sidebar.querySelectorAll("a[href]");
    for (var i = 0; i < links.length; i++) {
      links[i].addEventListener("click", persist);
    }
  }

  function markPendingCells() {
    var cells = document.querySelectorAll("table.matrix td");
    for (var i = 0; i < cells.length; i++) {
      var td = cells[i];
      if (td.textContent.trim() !== "Not yet") continue;
      td.classList.add("pending");
      td.title = "Known gap — not yet implemented for this framework.";
      td.setAttribute("data-pending", "matrix-gap");
    }
  }

  function adminHref() {
    if (location.hostname === "getrunkite.github.io") {
      return "https://getrunkite.com/admin/";
    }
    return "/admin/";
  }

  function injectLiveAdmin() {
    var href = adminHref();
    if (!document.getElementById("rk-live-admin-style")) {
      var style = document.createElement("style");
      style.id = "rk-live-admin-style";
      style.textContent =
        ".sidebar nav a.nav-live-admin{color:var(--accent);font-weight:600;box-shadow:inset 3px 0 0 var(--accent);margin-bottom:.85rem}" +
        ".topbar a[data-live-admin]{color:var(--accent-2);font-weight:600}";
      document.head.appendChild(style);
    }

    var topbar = document.querySelector(".topbar");
    if (topbar && !topbar.querySelector("[data-live-admin]")) {
      var top = document.createElement("a");
      top.href = href;
      top.setAttribute("data-live-admin", "1");
      top.textContent = "Live Admin";
      var product = null;
      var links = topbar.querySelectorAll("a[href]");
      for (var i = 0; i < links.length; i++) {
        var h = links[i].getAttribute("href") || "";
        if (h === "../" || h === "../../" || h === "../index.html") {
          product = links[i];
          break;
        }
      }
      if (product && product.nextSibling) {
        topbar.insertBefore(top, product.nextSibling);
      } else {
        topbar.insertBefore(top, topbar.firstChild);
      }
    }

    var nav = document.querySelector(".sidebar nav");
    if (nav && !nav.querySelector("[data-live-admin]")) {
      var side = document.createElement("a");
      side.href = href;
      side.setAttribute("data-live-admin", "1");
      side.className = "nav-live-admin";
      side.textContent = "Live Admin";
      nav.insertBefore(side, nav.firstChild);
    }
  }

  ready(function () {
    enhanceCodeBlocks();
    keepSidebarScroll();
    markPendingCells();
    injectLiveAdmin();
  });
})();
