(function () {
  if (document.getElementById('varroa-banner')) return;

  var script = document.currentScript;
  if (!script) return;

  var configURL = script.getAttribute('data-varroa-config');
  if (!configURL) return;

  var params = new URL('https://x/' + configURL).searchParams;
  var controller = params.get('controller') || '';
  var namespace = params.get('namespace') || '';
  var back = params.get('back') || '';

  // Identity attributes from server-side data-* (Task 5)
  var userId   = script.getAttribute('data-user-id')   || '';
  var userName = script.getAttribute('data-user-name') || '';
  var email    = script.getAttribute('data-user-email')|| '';
  var groups   = (script.getAttribute('data-user-groups')|| '').split(',').filter(Boolean);
  var role     = script.getAttribute('data-user-role') || '';
  var rootUrl  = (script.getAttribute('data-root-url') || '').replace(/\/+$/, '');
  var crumbField = script.getAttribute('data-crumb-field') || '';
  var crumbValue = script.getAttribute('data-crumb-value') || '';

  function miteChipHTML(connected) {
    if (connected) {
      return '<span style="display:inline-flex;align-items:center;gap:6px;font-size:11px;font-weight:600;padding:3px 9px;border-radius:7px;background:rgba(255,255,255,.10);color:#EAD7B5">' +
        '<span class="v-pulse on" style="position:relative;width:8px;height:8px;flex-shrink:0"><i style="position:absolute;inset:0;border-radius:50%;background:#2F8F4E"></i></span> mite linked</span>';
    }
    return '<span style="display:inline-flex;align-items:center;gap:6px;font-size:11px;font-weight:600;padding:3px 9px;border-radius:7px;background:rgba(255,255,255,.10);color:#EAD7B5"><span style="width:8px;height:8px;border-radius:50%;background:#8A7960;flex-shrink:0"></span> mite offline</span>';
  }

  // Compute initials from userName (Task 5)
  function computeInitials(name, id) {
    if (!name && id) {
      return id.substring(0, 2).toUpperCase();
    }
    var tokens = name.trim().split(/\s+/);
    if (tokens.length >= 2) {
      return (tokens[0].charAt(0) + tokens[1].charAt(0)).toUpperCase();
    }
    return name.substring(0, 2).toUpperCase();
  }

  var initials = userId ? computeInitials(userName, userId) : '';

  // Build the avatar element (or empty if anonymous)
  var avatarEl = null;
  if (userId) {
    avatarEl = document.createElement('div');
    avatarEl.id = 'varroa-avatar';
    avatarEl.setAttribute('role', 'button');
    avatarEl.setAttribute('tabindex', '0');
    avatarEl.style.cssText = 'width:30px;height:30px;border-radius:50%;background:#C2611C;color:#fff;display:grid;place-items:center;font-weight:700;font-size:11.5px;border:2px solid rgba(255,255,255,.22);cursor:pointer';
    avatarEl.textContent = initials;
  }

  // Build the banner HTML (the avatar div is appended separately so textContent works)
  var bannerHtml =
    '<div style="display:flex;align-items:center;gap:13px;padding:0 18px;height:54px;background:#241608;color:#F3E4C8;border-bottom:2px solid #C2611C;font-family:-apple-system,BlinkMacSystemFont,Inter,Segoe UI,sans-serif;font-size:14px;line-height:1.5">' +
      '<svg width="26" height="26"><polygon points="13,2 23,7.5 23,18.5 13,24 3,18.5 3,7.5" fill="#E0A21A"/><polygon points="13,7 19,10.5 19,15.5 13,19 7,15.5 7,10.5" fill="#241608"/><circle cx="13" cy="13" r="2" fill="#E0A21A"/></svg>' +
      '<span style="font-weight:700;font-size:15px;letter-spacing:-.2px;color:#FBEFD6">Varroa</span>' +
      '<span style="width:1px;height:22px;background:rgba(255,255,255,.16);flex-shrink:0"></span>' +
      '<span style="font-weight:600;font-size:13.5px;color:#F3E4C8">' + safeHTML(controller) + '</span>' +
      '<span id="varroa-mite-chip"></span>' +
      '<div style="margin-left:auto"></div>' +
      '<span style="font-size:11px;font-weight:600;padding:3px 9px;border-radius:7px;background:rgba(255,255,255,.10);color:#EAD7B5">ns/' + safeHTML(namespace) + '</span>' +
      (back ? '<a href="' + safeHTML(back) + '" style="font-size:12.5px;font-weight:600;color:#EBC07A;text-decoration:none;display:inline-flex;align-items:center;gap:6px">\u21a9 Control plane</a>' : '') +
    '</div>';

  var banner = document.createElement('div');
  banner.innerHTML = bannerHtml;

  // Append avatar element (built with textContent) to the banner bar
  if (avatarEl) {
    banner.firstChild.appendChild(avatarEl);
  }

  // Profile sub-menu panel (Task 6)
  var menuPanel = null;
  if (userId) {
    menuPanel = document.createElement('div');
    menuPanel.id = 'varroa-profile-menu';
    menuPanel.style.cssText = 'display:none;position:absolute;top:100%;right:12px;width:260px;background:#2A1C0E;border:1px solid rgba(255,255,255,.12);border-radius:8px;box-shadow:0 4px 16px rgba(0,0,0,.4);z-index:10000;font-family:-apple-system,BlinkMacSystemFont,Inter,Segoe UI,sans-serif;font-size:13px;color:#F3E4C8;overflow:hidden';

    // Header row: large initials + name + role badge
    var headerRow = document.createElement('div');
    headerRow.style.cssText = 'display:flex;align-items:center;gap:10px;padding:12px 14px;background:rgba(255,255,255,.04);border-bottom:1px solid rgba(255,255,255,.06)';

    var largeAvatar = document.createElement('div');
    largeAvatar.style.cssText = 'width:34px;height:34px;border-radius:50%;background:#C2611C;color:#fff;display:grid;place-items:center;font-weight:700;font-size:13px;flex-shrink:0';
    largeAvatar.textContent = initials;

    var nameRoleWrap = document.createElement('div');
    nameRoleWrap.style.cssText = 'display:flex;flex-direction:column;gap:2px;min-width:0';

    var nameSpan = document.createElement('div');
    nameSpan.style.cssText = 'font-weight:600;font-size:13.5px;color:#FBEFD6;overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
    nameSpan.textContent = userName;

    if (role) {
      var roleBadge = document.createElement('div');
      roleBadge.style.cssText = 'font-size:11px;font-weight:600;color:#EBC07A;background:rgba(194,97,28,.2);border-radius:4px;padding:1px 6px;display:inline-block;align-self:flex-start';
      roleBadge.textContent = role;
      nameRoleWrap.appendChild(nameSpan);
      nameRoleWrap.appendChild(roleBadge);
    } else {
      nameRoleWrap.appendChild(nameSpan);
    }

    headerRow.appendChild(largeAvatar);
    headerRow.appendChild(nameRoleWrap);
    menuPanel.appendChild(headerRow);

    // Email row (Task 6)
    if (email) {
      var emailRow = document.createElement('div');
      emailRow.style.cssText = 'display:flex;align-items:center;gap:8px;padding:8px 14px;border-bottom:1px solid rgba(255,255,255,.06)';
      var emailLabel = document.createElement('span');
      emailLabel.style.cssText = 'font-size:11px;font-weight:600;color:#8A7960;width:50px;flex-shrink:0';
      emailLabel.textContent = 'Email';
      var emailVal = document.createElement('span');
      emailVal.style.cssText = 'overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:#EAD7B5';
      emailVal.textContent = email;
      emailRow.appendChild(emailLabel);
      emailRow.appendChild(emailVal);
      menuPanel.appendChild(emailRow);
    }

    // Groups row (Task 6)
    if (groups.length > 0) {
      var groupsRow = document.createElement('div');
      groupsRow.style.cssText = 'display:flex;align-items:flex-start;gap:8px;padding:8px 14px;border-bottom:1px solid rgba(255,255,255,.06)';
      var groupsLabel = document.createElement('span');
      groupsLabel.style.cssText = 'font-size:11px;font-weight:600;color:#8A7960;width:50px;flex-shrink:0;padding-top:1px';
      groupsLabel.textContent = 'Groups';
      var groupsVal = document.createElement('span');
      groupsVal.style.cssText = 'font-size:12px;color:#EAD7B5;line-height:1.4;word-break:break-word';
      var displayGroups = groups.slice(0, 8);
      var groupsText = displayGroups.join(', ');
      if (groups.length > 8) {
        groupsText += ', +' + (groups.length - 8) + ' more';
      }
      groupsVal.textContent = groupsText;
      groupsRow.appendChild(groupsLabel);
      groupsRow.appendChild(groupsVal);
      menuPanel.appendChild(groupsRow);
    }

    // Divider before quick actions
    var divider = document.createElement('div');
    divider.style.cssText = 'height:1px;background:rgba(255,255,255,.06)';
    menuPanel.appendChild(divider);

    // Quick actions (Task 7)
    function addActionRow(linkEl) {
      var row = document.createElement('div');
      row.style.cssText = 'padding:0';
      var actionLink = linkEl;
      actionLink.style.cssText = 'display:block;padding:9px 14px;color:#F3E4C8;text-decoration:none;font-size:13px;font-weight:500';
      actionLink.addEventListener('mouseenter', function () {
        actionLink.style.background = 'rgba(255,255,255,.06)';
      });
      actionLink.addEventListener('mouseleave', function () {
        actionLink.style.background = '';
      });
      row.appendChild(actionLink);
      return row;
    }

    // View profile
    var profileLink = document.createElement('a');
    profileLink.href = rootUrl + '/user/' + encodeURIComponent(userId) + '/';
    profileLink.textContent = 'View profile';
    menuPanel.appendChild(addActionRow(profileLink));

    // Configure / API token
    var configLink = document.createElement('a');
    configLink.href = rootUrl + '/user/' + encodeURIComponent(userId) + '/configure';
    configLink.textContent = 'Configure / API token';
    menuPanel.appendChild(addActionRow(configLink));

    // Back to Varroa dashboard (only if back is configured)
    if (back) {
      var backLink = document.createElement('a');
      backLink.href = back;
      backLink.textContent = 'Back to Varroa dashboard';
      menuPanel.appendChild(addActionRow(backLink));
    }

    // Sign out (POST form with crumb)
    var signOutRow = document.createElement('div');
    signOutRow.style.cssText = 'padding:0';
    var signOutForm = document.createElement('form');
    signOutForm.method = 'post';
    signOutForm.action = rootUrl + '/logout';
    signOutForm.style.cssText = 'display:block';
    if (crumbField) {
      var crumbInput = document.createElement('input');
      crumbInput.type = 'hidden';
      crumbInput.name = crumbField;
      crumbInput.value = crumbValue;
      signOutForm.appendChild(crumbInput);
    }
    var signOutBtn = document.createElement('button');
    signOutBtn.type = 'submit';
    signOutBtn.textContent = 'Sign out';
    signOutBtn.style.cssText = 'display:block;width:100%;padding:9px 14px;background:none;border:none;color:#EBC07A;text-decoration:none;font-size:13px;font-weight:500;cursor:pointer;text-align:left;font-family:inherit';
    signOutBtn.addEventListener('mouseenter', function () {
      signOutBtn.style.background = 'rgba(255,255,255,.06)';
    });
    signOutBtn.addEventListener('mouseleave', function () {
      signOutBtn.style.background = '';
    });
    signOutForm.appendChild(signOutBtn);
    signOutRow.appendChild(signOutForm);
    menuPanel.appendChild(signOutRow);
  }

  var style = document.createElement('style');
  style.textContent = '.v-pulse.on i::after,.v-pulse.on i::before{content:"";position:absolute;inset:0;border-radius:50%;border:2px solid #2F8F4E;animation:vping 1.9s cubic-bezier(0,0,.2,1) infinite}.v-pulse.on i::before{animation-delay:.95s}@keyframes vping{0%{transform:scale(1);opacity:.7}80%,100%{transform:scale(3);opacity:0}}';
  document.head.appendChild(style);

  // Store reference before insertion (banner.firstChild is moved out). The bar is the node
  // actually inserted into the page, so it carries the id the re-injection guard checks.
  var bannerBar = banner.firstChild;
  bannerBar.id = 'varroa-banner';

  function install() {
    var target = document.querySelector('#page-header, #header');
    var isNewUX = !!(target && target.classList &&
                     target.classList.contains('jenkins-header'));

    if (target && target.parentNode) {
      target.parentNode.insertBefore(bannerBar, target);
    } else {
      document.body.insertBefore(bannerBar, document.body.firstChild);
    }

    // Position the bar. The redesigned Jenkins UI (2.5xx) ships a sticky header
    // (#page-header.jenkins-header, top:0, z-index 999) that otherwise paints over
    // our in-flow bar. Make our bar the top-most sticky element and push the
    // Jenkins header's sticky pin-point down by our height so the two stack.
    if (isNewUX) {
      bannerBar.style.position = 'sticky';
      bannerBar.style.top = '0';
      bannerBar.style.zIndex = '1000';
      target.style.top = bannerBar.offsetHeight + 'px';
    } else if (menuPanel) {
      // Classic UI: relative makes the bar the positioning parent for the menu.
      bannerBar.style.position = 'relative';
    }

    // Attach the profile menu (anchored to the now-positioned bar).
    if (menuPanel && bannerBar) {
      bannerBar.appendChild(menuPanel);
    }

    // Dismissal behavior (Task 8)
    if (avatarEl && menuPanel) {
      avatarEl.addEventListener('click', function (e) {
        e.stopPropagation();
        var isOpen = menuPanel.style.display !== 'none';
        menuPanel.style.display = isOpen ? 'none' : 'block';
      });

      document.addEventListener('click', function (e) {
        if (menuPanel.style.display === 'none') return;
        if (e.target === avatarEl || avatarEl.contains(e.target)) return;
        if (e.target === menuPanel || menuPanel.contains(e.target)) return;
        menuPanel.style.display = 'none';
      });

      document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape' && menuPanel.style.display !== 'none') {
          menuPanel.style.display = 'none';
        }
      });
    }
  }

  if (document.body) {
    install();
  } else {
    document.addEventListener('DOMContentLoaded', install);
  }

  function safeHTML(s) {
    return String(s).replace(/[<>&"']/g, function (c) {
      switch (c) {
        case '<': return '&lt;';
        case '>': return '&gt;';
        case '&': return '&amp;';
        case '"': return '&quot;';
        case "'": return '&#39;';
        default: return c;
      }
    });
  }

  function refreshMiteStatus() {
    fetch('/userContent/varroa-operator-status.json?_=' + Date.now())
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        var chip = document.getElementById('varroa-mite-chip');
        if (chip && d !== null) chip.innerHTML = miteChipHTML(d.connected === true);
      })
      .catch(function () {});
  }

  refreshMiteStatus();
  setInterval(refreshMiteStatus, 15000);
})();
