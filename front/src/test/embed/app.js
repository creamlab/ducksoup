const state = {};

// "1" -> true
const toBool = (v) => Boolean(parseInt(v));

const getQueryVariable = (key, deserializeFunc) => {
    const query = window.location.search.substring(1);
    const vars = query.split("&");
    for (let i = 0; i < vars.length; i++) {
        const pair = vars[i].split("=");
        if (decodeURIComponent(pair[0]) == key) {
            const value = decodeURIComponent(pair[1]);
            return deserializeFunc ? deserializeFunc(value) : value;
        }
    }
};

const marshallParams = (obj) => encodeURI(btoa(JSON.stringify(obj)));

const init = async () => {
    const roomId = getQueryVariable("roomId");
    const userId = getQueryVariable("userId");
    const videoCodec = getQueryVariable("videoCodec");
    const duration = getQueryVariable("duration", (v) => parseInt(v, 10));
    if (typeof roomId === 'undefined' || typeof userId === 'undefined' || isNaN(duration)) {
        document.getElementById("error").classList.remove("d-none");
        document.getElementById("embed").classList.add("d-none");
    } else {
        const params = {
            origin: window.location.origin,
            roomId,
            userId,
            duration,
            videoCodec
        };
        state.userId = userId;
        document.getElementById("embed").src = `/embed/?params=${marshallParams(params)}`;
    }
};

document.addEventListener("DOMContentLoaded", init);

const hideEmbed = () => {
    document.getElementById("stopped").classList.remove("d-none");
    document.getElementById("embed").classList.add("d-none");
}

const replaceMessage = (message) => {
    document.getElementById("stopped-message").innerHTML = message;
    hideEmbed();
}

const appendMessage = (message) => {
    document.getElementById("stopped-message").innerHTML += '<br/>' + message;
    hideEmbed();
}

// communication with iframe
window.addEventListener("message", (event) => {
    if (event.origin !== window.location.origin) return;

    const { kind, payload } = event.data;
    if (event.data.kind === "end") {
        if(payload && payload[state.userId]) {
            let html = "Conversation terminée, les fichiers suivant ont été enregistrés :<br/><br/>";
            html += payload[state.userId].join("<br/>");
            replaceMessage(html);
        } else {
            replaceMessage("Conversation terminée");
        }
    } else if (kind === "error-full") {
        replaceMessage("Connexion refusée (salle complète)");
    } else if (kind === "error-duplicate") {
        replaceMessage("Connexion refusée (déjà connecté-e)");
    } else if (kind === "disconnection") {
        appendMessage("Connexion perdue");
    } else if (kind === "error") {
        replaceMessage("Erreur");
    }
});