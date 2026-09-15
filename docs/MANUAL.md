# Manual de utilização

Este manual descreve o produto como ele se comporta hoje, verificado usando a
aplicação, não lendo o código. Onde há limitação, ela está escrita.

---

## 1. O que a ferramenta faz

Você entrega um documento. Ela lê o conteúdo — inclusive de digitalizações, via
OCR —, encontra os dados pessoais lá dentro (CPF, CNPJ, cartão, IBAN,
Steuer-ID, e-mail, telefone) e devolve um relatório dizendo quais obrigações de
GDPR e LGPD aquilo dispara.

**O que ela não faz.** Não decide por você se um compartilhamento é legal, não
guarda o documento original depois da análise, e não encontra dado pessoal que
não tenha forma reconhecível — um nome solto numa frase não é detectado. O
relatório é um ponto de partida para uma decisão humana, não a decisão.

---

## 2. Os três trabalhos

Tudo na tela existe para um destes três. Saber qual você está fazendo é o que
torna a interface previsível.

### Inventário de dados pessoais
*"Que dados pessoais minha organização tem?"* — a pergunta do Art. 30 do GDPR e
do Art. 37 da LGPD.

Envie tudo o que quiser mapear e leia o painel **Inventário de dados pessoais**.
Ele responde no nível do acervo: quantos documentos contêm dado pessoal, quantos
são restritos, e quais categorias aparecem em quantos documentos.

### Triagem antes de compartilhar
*"Posso mandar este arquivo para fora?"*

Envie o documento, abra-o e leia o veredito no topo do painel de detalhe. Os
filtros da lista (**Restritos**, **Dados pessoais**, **Livres**) existem para
separar um lote em "pode ir" e "não pode ir".

### Cofre com direito ao esquecimento
*"Apague o que vocês têm sobre mim."* — Art. 17 do GDPR, Art. 18 da LGPD.

Busque pelo nome do arquivo, abra e use **Apagar (Art. 17)**. Só um admin pode.

---

## 3. Quem usa e o que cada um pode

Três papéis, em escada: cada um pode tudo o que o anterior podia, mais alguma
coisa.

| | Viewer | Analyst | Admin |
|---|:---:|:---:|:---:|
| Ver documentos, relatórios e o inventário | ✅ | ✅ | ✅ |
| Ver o histórico de execução do pipeline | ✅ | ✅ | ✅ |
| Trocar a própria senha | ✅ | ✅ | ✅ |
| Enviar documentos | ❌ | ✅ | ✅ |
| Apagar documentos (Art. 17) | ❌ | ❌ | ✅ |
| Criar, remover e redefinir senha de contas | ❌ | ❌ | ✅ |

**Quem é quem, na prática:**

- **Viewer** — auditoria, jurídico, um DPO que precisa consultar sem alterar
  nada. Note que um viewer **vê os achados**: a lista de dados pessoais
  encontrados. Ele é read-only, não é cego.
- **Analyst** — quem faz o trabalho: envia os documentos e lê os relatórios.
- **Admin** — responde pelas contas e é o único que pode apagar. Apagar é
  irreversível, e por isso não é um poder do dia a dia.

Esses limites são aplicados **no servidor**, não escondendo botões: um viewer
que chame a API diretamente para enviar ou apagar recebe `403`.

---

## 4. Começando do zero

Não existe cadastro público. A primeira conta nasce por linha de comando; todas
as outras nascem dentro da aplicação, criadas por um admin.

```bash
go run ./cmd/seed-admin -org "Sua Empresa" -email "voce@empresa.com"
```

Sem `-password`, ele gera uma e imprime uma única vez.

Depois, entre e use **Usuários** no topo da tela para criar as demais contas. A
senha inicial aparece **uma vez só** — o servidor guarda apenas o hash, e não
existe como recuperá-la depois. Entregue por um canal seguro e peça a troca no
primeiro acesso.

Redefinir a senha de alguém **encerra as sessões abertas daquela pessoa**. Tanto
redefinir quanto remover pedem confirmação na própria linha, porque os dois são
irreversíveis e ficam lado a lado.

---

## 5. O ciclo de um documento

Enviar um arquivo cria uma tarefa que passa por três estágios: **OCR** (extrai o
texto) → **Detecção** (encontra identificadores) → **Relatório** (mapeia
obrigações). O painel de detalhe mostra os três, com o tempo de cada um.

Um documento pode estar em um destes estados:

| Estado na tela | Significa |
|---|---|
| **Analisando** | ainda passando pelos estágios. É transitório. |
| **Livre** | analisado, nenhum identificador encontrado. |
| **Dados pessoais** | analisado, contém identificadores comuns. |
| **Restrito** | contém dado financeiro ou sensível. Exige base legal e segurança reforçada (GDPR Art. 32/46, LGPD Art. 46). |
| **Não analisado** | o pipeline falhou. O badge diz em que estágio. **Exposição desconhecida.** |
| **Sem relatório** | consta como analisado, mas o relatório não pôde ser lido. Também **exposição desconhecida** — trate como não verificado. |

Quando um documento fica **Não analisado**, o painel de detalhe diz o **motivo**
em linguagem comum (por exemplo, "o PDF está corrompido") e **o que fazer** —
em geral, conferir o arquivo e enviá-lo de novo. Não existe botão de
reprocessar: um arquivo que falhou por estar ilegível falharia outra vez. O erro
técnico registrado fica em **Histórico de tentativas**, recolhido, para quem
precisar diagnosticar.

Os dois últimos são a mesma advertência com causas diferentes, e ambos contam no
cartão **Não analisados** do inventário. Um documento nunca fica em dois estados
ao mesmo tempo: analisados + não analisados + em processamento sempre soma o
total do acervo.

---

## 6. O que a ferramenta apaga sozinha

Depois que o relatório fica pronto, **o original e o texto extraído são
destruídos**. O painel de detalhe diz a data e a hora em que isso aconteceu. O
que sobra é o relatório e a lista de achados, que registram *que tipo* de dado
foi encontrado e onde, mas **não guardam nenhum valor bruto** — o CPF em si não
fica.

Isso é deliberado: uma ferramenta que rastreia dados pessoais não deveria virar
mais uma cópia deles.

Um documento que **falhou** nunca chega a ter relatório. O original dele fica
guardado por um prazo curto — 24 horas na configuração padrão —, para permitir
diagnosticar a falha, e depois é destruído automaticamente. Apagar (Art. 17)
remove antes disso.

**Apagar (Art. 17)** vai além e remove também o relatório e o registro.

---

## 7. Senhas

Qualquer pessoa troca a própria senha em **Senha**, no topo. Ao trocar, **todas
as sessões caem, inclusive a sua** — você volta para a tela de entrada e refaz o
login com a senha nova. É proposital: uma sessão roubada antes da troca não pode
sobreviver a ela.

A sessão dura 12 horas. Quando ela expira com a tela aberta, a aplicação volta
para a tela de entrada e avisa que a sessão expirou.

Senhas iniciais criadas por um admin **não** obrigam troca no primeiro acesso —
cabe a quem recebeu trocá-la em **Senha**.

Esqueceu? **Esqueci minha senha** na tela de entrada envia um link por e-mail. A
tela responde sempre a mesma coisa, exista ou não a conta — isso é intencional,
para não revelar quem tem conta. O link vale por tempo limitado e só pode ser
usado uma vez; abrir um link vencido ou já usado oferece **Pedir um novo link**.

---

## 8. Limitações conhecidas

Escritas porque a alternativa é você descobrir sozinho num documento que
importava.

- **Telefone é "melhor esforço", sem dígito verificador.** Formatos brasileiros
  `(11) 98765-4321`, `11 98765-4321` e fixos são reconhecidos, mas telefone não
  tem como ser validado, então erros nos dois sentidos são possíveis.
- **Um celular escrito sem separador nenhum pode virar "Steuer-ID (DE)".**
  `11987654321` tem onze dígitos, exatamente a forma de um CPF ou de um
  Steuer-ID alemão; quando o dígito verificador fecha por acaso, a categoria
  validada ganha. O documento continua marcado como contendo dado pessoal — o
  que muda é o rótulo da categoria, não o veredito.
- **Nomes não são detectados.** Só identificadores com forma reconhecível.
- **OCR erra.** Em digitalização ruim, um dígito lido errado faz o CPF falhar na
  validação e ele não aparece no relatório. Um documento "Livre" que você sabe
  conter dado pessoal provavelmente é um problema de leitura, não de detecção.
- **O limite de envio é 25 MB** e PDFs têm limite de páginas.

---

## 9. Web e desktop

A versão **web** é multiusuário, tem login e papéis, e os documentos passam pela
infraestrutura de quem hospeda.

A versão **desktop** é a mesma aplicação rodando inteira na máquina do usuário:
banco em arquivo local, nada escutando fora do `localhost`, nenhum documento
saindo do computador. Por isso ela **não tem login nem papéis** — não há contra
quem autenticar, e o login do sistema operacional é a fronteira. Tudo o que a
seção 3 diz sobre papéis vale só para a versão web.

Instalação e detalhes do instalador: [`desktop/README.md`](../desktop/README.md).
