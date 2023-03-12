// assets
import { GithubOutlined, QuestionOutlined } from '@ant-design/icons';
import DiscordOutlined from '../assets/images/icons/discord.svg'

// ==============================|| MENU ITEMS - SAMPLE PAGE & DOCUMENTATION ||============================== //

const DiscordOutlinedIcon = (props) => {
    return (
        <img src={DiscordOutlined} width="16px" alt="Discord" {...props} />
    );
};

const support = {
    id: 'support',
    title: 'Support',
    type: 'group',
    children: [
        {
            id: 'discord',
            title: 'Discord',
            type: 'item',
            url: 'https://discord.com/invite/PwMWwsrwHA',
            icon: DiscordOutlinedIcon,
            external: true,
            target: true
        },
        {
            id: 'github',
            title: 'Github',
            type: 'item',
            url: 'https://github.com/azukaar/Cosmos-Server',
            icon: GithubOutlined,
            external: true,
            target: true
        },
        {
            id: 'documentation',
            title: 'Documentation',
            type: 'item',
            url: 'https://github.com/azukaar/Cosmos-Server/wiki',
            icon: QuestionOutlined,
            external: true,
            target: true
        }
    ]
};

export default support;