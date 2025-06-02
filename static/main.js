document.addEventListener("DOMContentLoaded", () => {
  /* ---------------- DOM refs ---------------- */
  const urlInput   = document.getElementById("urlInput");
  const infoBtn    = document.getElementById("infoBtn");
  const typeSel    = document.getElementById("typeSelect");
  const qualitySel = document.getElementById("qualitySelect");
  const qualityRow = document.getElementById("qualityRow");
  const langSel    = document.getElementById("langSelect");
  const langRow    = document.getElementById("langRow");
  const actionBtn  = document.getElementById("actionBtn");
  const progressBox= document.getElementById("progressContainer");
  const stageSpan  = document.getElementById("stageText");
  const bar        = document.getElementById("progress");
  const resultP    = document.getElementById("result");
  const thumbImg   = document.getElementById("thumbPreview");
  const settingsBtn= document.getElementById("settingsBtn");
  const dialog     = document.getElementById("settingsDialog");
  const cookiesTA  = document.getElementById("cookiesArea");
  const closeSet   = document.getElementById("closeSettings");
  const clearBtn   = document.getElementById("clearCookies");

  /* ------------- toast ---------------- */
  function toast(msg, ok=true){
    const div=document.createElement("div");
    div.className="toast "+(ok?"success":"error");
    div.textContent=msg;
    document.body.append(div);
    setTimeout(()=>div.remove(),3500);
  }

  /* ------------- cookies localStorage ---- */
  cookiesTA.value = localStorage.getItem("ytCookies") || "";
  cookiesTA.oninput = () => localStorage.setItem("ytCookies", cookiesTA.value.trim());
  clearBtn.onclick  = () => { cookiesTA.value=""; cookiesTA.oninput(); toast("Cookies eliminadas") };

  settingsBtn.onclick = () => dialog.showModal();
  closeSet.onclick    = () => dialog.close();

  /* ------------- estado runtime ---------- */
  let lastInfo=null, currentJob=null, es=null;
  const getCookies = () => cookiesTA.value.trim();

  /* ------------- UI helpers -------------- */
  function populateQualities(){
    qualitySel.innerHTML='<option value="">Auto</option>';
    if(!lastInfo) return;
    const list = typeSel.value==="audio" ? lastInfo.audio_qualities : lastInfo.video_qualities;
    list.forEach(val=>{
      const label = typeSel.value==="audio" ? `${val} kbps` : `${val}p`;
      qualitySel.insertAdjacentHTML("beforeend",`<option value="${val}">${label}</option>`);
    });
  }

  function toggleRows(){
    const t=typeSel.value;
    qualityRow.classList.toggle("hidden", t==="subs"||t==="thumb");
    langRow.classList.toggle("hidden", t!=="subs");
    thumbImg.classList.toggle("hidden", t!=="thumb"||!lastInfo);
    if(t==="thumb"&&lastInfo) thumbImg.src=lastInfo.thumb_url;
    populateQualities();
  }
  typeSel.onchange = toggleRows;

  function resetUI(msg){
    currentJob=null;
    actionBtn.textContent="Descargar";
    actionBtn.dataset.mode="start";
    progressBox.classList.add("hidden");
    stageSpan.textContent="";
    resultP.innerHTML=msg||"";
  }

  /* ------------- Obtener info ------------ */
  infoBtn.onclick = async ()=>{
    const url=urlInput.value.trim();
    if(!url) return toast("Introduce la URL",false);
    infoBtn.disabled=true;

    const fd=new FormData();
    fd.append("url",url);
    fd.append("cookies",getCookies());
    const r=await fetch("/info",{method:"POST",body:fd});
    infoBtn.disabled=false;

    if(!r.ok){
      const {error}=await r.json().catch(()=>({error:"desconocido"}));
      toast("Error: "+error,false); return;
    }
    lastInfo=await r.json();
    populateQualities();
    langSel.innerHTML="";
    lastInfo.sub_langs.forEach(l=>langSel.insertAdjacentHTML("beforeend",`<option value="${l}">${l}</option>`));
    thumbImg.src=lastInfo.thumb_url;
    toggleRows();
    toast("Datos obtenidos satisfactoriamente");
  };

  /* ------------- Descargar / Cancelar ---- */
  async function startDownload(){
    actionBtn.textContent="Cancelar";
    actionBtn.dataset.mode="cancel";
    resultP.textContent="";
    bar.style.width="0%";
    stageSpan.textContent="Iniciando…";
    progressBox.classList.remove("hidden");

    const fd=new FormData();
    fd.append("url",urlInput.value.trim());
    fd.append("type",typeSel.value);
    fd.append("quality",qualitySel.value);
    fd.append("sub_lang",langSel.value);
    fd.append("cookies",getCookies());

    const res=await fetch("/download",{method:"POST",body:fd});
    const {job,error}=await res.json();
    if(error){ resetUI(error); toast("Error: "+error,false); return }
    currentJob=job;
    es=new EventSource(`/progress/${job}`);

    es.onmessage = ev => { bar.style.width = parseInt(ev.data, 10) + "%"; };

    es.addEventListener("stage", ev => { stageSpan.textContent = ev.data; });

    es.addEventListener("ready", ev => {
      es.close();
      stageSpan.textContent = "Completado ✔";
      resetUI(`<a href="${ev.data}" target="_blank">Descargar archivo</a>`);
      toast("Descarga completa ✔");
    });

    es.addEventListener("error", ev=>{
      es.close(); resetUI(ev.data||"Error"); toast(ev.data||"Error",false)
    });
    es.onerror = ()=>{ es.close(); resetUI("Conexión SSE perdida"); toast("Conexión perdida",false) };
  }

  async function cancelDownload(){
    if(!currentJob) return;
    await fetch(`/cancel/${currentJob}`,{method:"POST"});
    if(es) es.close();
    resetUI("Descarga cancelada");
    toast("Descarga cancelada",false);
  }

  actionBtn.dataset.mode="start";
  actionBtn.onclick=()=> actionBtn.dataset.mode==="start" ? startDownload() : cancelDownload();
});
