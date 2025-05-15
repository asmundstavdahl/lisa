document.addEventListener('DOMContentLoaded', () => {
    const messageInput = document.getElementById('message-input');
    const sendButton = document.getElementById('send-button');
    const chatHistory = document.getElementById('chat-history');

    // Funksjon for å legge til melding i chat-historikken
    function addMessageToHistory(message, sender, timestamp) {
        const messageElement = document.createElement('div');
        messageElement.classList.add('message');
        messageElement.classList.add(sender.toLowerCase() === 'user' ? 'user-message' : 'lisa-message');
        
        const messageContent = document.createElement('span');
        messageContent.textContent = message;
        messageElement.appendChild(messageContent);

        if (timestamp) {
            const timeElement = document.createElement('span');
            timeElement.classList.add('timestamp');
            timeElement.textContent = new Date(timestamp).toLocaleTimeString('nb-NO', { hour: '2-digit', minute: '2-digit' });
            messageElement.appendChild(timeElement);
        }
        
        chatHistory.appendChild(messageElement);
        chatHistory.scrollTop = chatHistory.scrollHeight; // Scroll til bunnen
    }

    // Funksjon for å hente og vise historikk
    async function fetchHistory() {
        try {
            const response = await fetch('/get_history');
            if (!response.ok) {
                throw new Error(`HTTP error! status: ${response.status}`);
            }
            const messages = await response.json();
            chatHistory.innerHTML = ''; // Tøm tidligere historikk
            if (messages && messages.length > 0) {
                messages.forEach(msg => {
                    addMessageToHistory(msg.message, msg.sender, msg.timestamp);
                });
            } else {
                 addMessageToHistory('Velkommen til LISA! Hvordan kan jeg hjelpe deg i dag?', 'lisa', new Date());
            }
        } catch (error) {
            console.error('Kunne ikke hente historikk:', error);
            addMessageToHistory('Feil ved henting av historikk.', 'lisa', new Date());
        }
    }

    // Funksjon for å sende melding til backend
    async function sendMessage() {
        const messageText = messageInput.value.trim();
        if (messageText === '') return;

        addMessageToHistory(messageText, 'user', new Date()); // Vis brukerens melding umiddelbart
        messageInput.value = '';

        try {
            const response = await fetch('/send_message', {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/x-www-form-urlencoded',
                },
                body: `message=${encodeURIComponent(messageText)}`
            });

            if (!response.ok) {
                throw new Error(`HTTP error! status: ${response.status}`);
            }

            const data = await response.json();
            if (data.response && data.sender) {
                 addMessageToHistory(data.response, data.sender, new Date());
            } else {
                addMessageToHistory('Mottok et uventet svar fra serveren.', 'lisa', new Date());
            }

        } catch (error) {
            console.error('Feil ved sending av melding:', error);
            addMessageToHistory('Kunne ikke sende meldingen. Prøv igjen.', 'lisa', new Date());
        }
    }

    sendButton.addEventListener('click', sendMessage);
    messageInput.addEventListener('keypress', (event) => {
        if (event.key === 'Enter') {
            sendMessage();
        }
    });

    // Hent historikk når siden lastes
    fetchHistory();
});

