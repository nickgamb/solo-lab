(function () {
    function copy(text) {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        return navigator.clipboard.writeText(text).catch(fallback);
      }
      return fallback();

      function fallback() {
        var ta = document.createElement("textarea");
        ta.value = text;
        ta.setAttribute("readonly", "");
        ta.style.position = "fixed";
        ta.style.top = "-1000px";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.select();
        ta.setSelectionRange(0, ta.value.length);
        try { document.execCommand("copy"); } catch (e) { /* selection is still there to copy by hand */ }
        document.body.removeChild(ta);
        return Promise.resolve();
      }
    }

    document.querySelectorAll(".cmdwrap").forEach(function (wrap) {
      var code = wrap.querySelector("code.cmd");
      if (!code) return;
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "copy";
      btn.textContent = "Copy";
      btn.setAttribute("aria-label", "Copy command to clipboard");
      btn.addEventListener("click", function () {
        copy(code.textContent.trim()).then(function () {
          btn.textContent = "Copied";
          btn.classList.add("done");
          setTimeout(function () {
            btn.textContent = "Copy";
            btn.classList.remove("done");
          }, 1400);
        });
      });
      wrap.appendChild(btn);
    });
  })();
