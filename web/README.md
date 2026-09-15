# TaskFlow Compliance — interface

React + TypeScript + Vite. Em produção é compilada pelo `Dockerfile.web` e
servida pelo mesmo nginx que faz proxy de `/api`.

## Para que serve

Você tem uma pilha de documentos — contratos, formulários, digitalizações — e
não sabe que dados pessoais estão lá dentro. A interface responde três
perguntas, nesta ordem:

1. **Descobrir** — que dados pessoais o acervo contém? O painel de inventário
   agrega os relatórios de todos os documentos e mostra a exposição por
   categoria. É a pergunta de registro das atividades de tratamento
   (GDPR Art. 30 / LGPD Art. 37).
2. **Julgar** — este documento pode ser compartilhado? Cada documento recebe um
   veredito derivado do relatório, com as obrigações que ele dispara.
3. **Encerrar** — apagar tudo, de forma comprovável. O diálogo de exclusão lista
   os objetos que a sequência C4 remove antes de pedir confirmação.

Essa ordem é a arquitetura da informação da tela; mudá-la sem mudar o
enquadramento deixa a interface parecendo um painel de controle sem propósito.

## Decisões que não são óbvias no código

- **O veredito é derivado, não armazenado.** `lib/compliance.ts` traduz o
  relatório em `clear | personal | sensitive | unknown | pending`. Identificadores
  financeiros (cartão, IBAN) e categorias `special` elevam para `sensitive`,
  espelhando a ênfase que o próprio backend dá ao Art. 32 / Art. 46.
- **O inventário agrega no cliente.** Não existe endpoint de agregação porque os
  achados vivem em `findings.json` no object storage, não em tabela — não há
  `GROUP BY` possível. `useReports` busca os relatórios em lotes de 4 e os
  memoriza (um relatório é imutável depois de gerado). Se o acervo crescer muito,
  a correção certa é uma tabela `document_findings` escrita pelo estágio de PII,
  não mais paralelismo aqui.
- **Cor nunca carrega significado sozinha.** Os tons de status vêm de uma escala
  reservada em que `warning` e `serious` ficam abaixo de 3:1 no tema claro por
  desenho; por isso todo badge leva glifo e rótulo textual.
- **O gráfico de exposição usa uma cor só.** É uma métrica (documentos) sobre
  categorias nominais — o comprimento da barra já codifica o valor, e colorir
  cada barra gastaria o canal de identidade sem dizer nada.
- **A exclusão é um modal, não `confirm()`.** O diálogo nativo não consegue
  mostrar o que será destruído e é inalcançável para verificação automatizada.

## Comandos

```bash
npm install
npm run dev      # servidor de desenvolvimento, com proxy de /api para :8080
npm run build    # tsc -b && vite build → dist/
npm run lint
```
